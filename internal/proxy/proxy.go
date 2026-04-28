package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccg/internal/config"
	"ccg/internal/logger"
	"ccg/internal/storage"
)

// SSEEvent represents a Server-Sent Event
type SSEEvent struct {
	Event string
	Data  string
}

// Usage represents token usage information from API response
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// APIResponse represents the structure of API responses to extract usage
type APIResponse struct {
	Usage Usage `json:"usage"`
}

// CircuitBreakerState represents the state of a circuit breaker
type CircuitBreakerState int

const (
	CircuitClosed CircuitBreakerState = iota
	CircuitOpen
	CircuitHalfOpen
)

func (s CircuitBreakerState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// startDrainingEndpoint 开始排空指定端点，新请求将不再路由到该端点
// 已在该端点上的请求将等待完成或超时
func (p *Proxy) startDrainingEndpoint(endpointName string) {
	p.drainMu.Lock()
	defer p.drainMu.Unlock()

	deadline := time.Now().Add(p.drainTimeout)
	p.drainingEndpoints[endpointName] = deadline
	logger.Info("[Drain] 端点 %s 开始排空，截止时间: %v", endpointName, deadline.Format("15:04:05"))
}

// stopDrainingEndpoint 停止排空指定端点
func (p *Proxy) stopDrainingEndpoint(endpointName string) {
	p.drainMu.Lock()
	defer p.drainMu.Unlock()

	if _, exists := p.drainingEndpoints[endpointName]; exists {
		delete(p.drainingEndpoints, endpointName)
		logger.Debug("[Drain] 端点 %s 排空已停止", endpointName)
	}
}

// isDraining 检查端点是否正在排空
func (p *Proxy) isDraining(endpointName string) bool {
	p.drainMu.RLock()
	defer p.drainMu.RUnlock()

	if deadline, exists := p.drainingEndpoints[endpointName]; exists {
		if time.Now().After(deadline) {
			return false
		}
		return true
	}
	return false
}

// cleanupStaleDrainingEndpoints 清理过期的排空端点
func (p *Proxy) cleanupStaleDrainingEndpoints() int {
	p.drainMu.Lock()
	defer p.drainMu.Unlock()

	now := time.Now()
	cleaned := 0
	for endpointName, deadline := range p.drainingEndpoints {
		if now.After(deadline) {
			delete(p.drainingEndpoints, endpointName)
			cleaned++
			logger.Debug("[Drain] 端点 %s 排空已过期，已清理", endpointName)
		}
	}
	return cleaned
}

// GetDrainingEndpoints 返回当前正在排空的端点列表
func (p *Proxy) GetDrainingEndpoints() []string {
	p.drainMu.RLock()
	defer p.drainMu.RUnlock()

	result := make([]string, 0, len(p.drainingEndpoints))
	now := time.Now()
	for endpointName, deadline := range p.drainingEndpoints {
		if now.Before(deadline) {
			result = append(result, endpointName)
		}
	}
	return result
}

// CircuitBreaker tracks failures for an endpoint and can open the circuit
type CircuitBreaker struct {
	mu                sync.RWMutex
	failures          int           // consecutive failure count for service errors (503, etc.)
	networkFailures   int           // consecutive failure count for network errors (DNS, connection refused)
	threshold         int           // number of service failures before opening circuit
	networkThreshold  int           // number of network failures before opening circuit
	halfOpenSuccesses int           // successful calls needed to close circuit from half-open
	halfOpenRequired  int           // successful calls needed to close circuit
	openTime          time.Time     // when the circuit was opened
	recoveryTimeout   time.Duration // how long to wait before trying again
	state             CircuitBreakerState
}

// ShouldAllow checks if a request should be allowed through
func (cb *CircuitBreaker) ShouldAllow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if time.Since(cb.openTime) > cb.recoveryTimeout {
			cb.state = CircuitHalfOpen
			cb.halfOpenSuccesses = 0
			logger.Debug("[CircuitBreaker] Transitioning to half-open state")
			return true
		}
		return false
	case CircuitHalfOpen:
		return true
	default:
		return true
	}
}

// RecordSuccess records a successful request
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		cb.failures = 0
		cb.networkFailures = 0
	case CircuitHalfOpen:
		cb.halfOpenSuccesses++
		if cb.halfOpenSuccesses >= cb.halfOpenRequired {
			cb.state = CircuitClosed
			cb.failures = 0
			cb.networkFailures = 0
			cb.halfOpenSuccesses = 0
			logger.Info("[CircuitBreaker] Circuit closed, service recovered")
		}
	}
}

// RecordFailure records a failed request
// isNetworkError indicates if the failure is due to network issues (DNS, connection refused, etc.)
func (cb *CircuitBreaker) RecordFailure(isNetworkError bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		if isNetworkError {
			cb.networkFailures++
			cb.failures++ // also increment general counter for mixed error scenarios
			if cb.networkFailures >= cb.networkThreshold {
				cb.state = CircuitOpen
				cb.openTime = time.Now()
				logger.Warn("[CircuitBreaker] Circuit opened due to %d consecutive network failures (threshold: %d)", cb.networkFailures, cb.networkThreshold)
			}
		} else {
			cb.failures++
			cb.networkFailures++ // also increment network counter for mixed error scenarios
			if cb.failures >= cb.threshold {
				cb.state = CircuitOpen
				cb.openTime = time.Now()
				logger.Warn("[CircuitBreaker] Circuit opened due to %d consecutive service failures", cb.failures)
			}
		}
	case CircuitHalfOpen:
		cb.state = CircuitOpen
		cb.openTime = time.Now()
		logger.Warn("[CircuitBreaker] Request failed in half-open state, circuit reopened")
	}
}

// IsNetworkError determines if an error is a network-related error
func IsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	networkErrorIndicators := []string{
		"no such host",
		"connection refused",
		"connection reset",
		"connection aborted",
		"network is unreachable",
		"host is unreachable",
		"network is down",
		"cannot resolve",
		"lookup",
	}
	for _, indicator := range networkErrorIndicators {
		if strings.Contains(message, indicator) {
			return true
		}
	}
	return false
}

// Proxy represents the proxy server
type Proxy struct {
	config            *config.Config
	storage           *storage.SQLiteStorage
	stats             *Stats
	currentIndex      int
	mu                sync.RWMutex
	server            *http.Server
	httpClient        *http.Client                  // Reusable HTTP client with connection pool
	activeRequests    map[string]bool               // tracks active requests by endpoint name
	activeRequestsMu  sync.RWMutex                  // protects activeRequests map
	endpointCtx       map[string]context.Context    // context per endpoint for cancellation
	endpointCancel    map[string]context.CancelFunc // cancel functions per endpoint
	ctxMu             sync.RWMutex                  // protects context maps
	onEndpointSuccess func(endpointName string)     // callback when endpoint request succeeds
	modelsCache       *ModelsCache                  // Cache for /v1/models endpoint
	resolver          *EndpointResolver             // 端点解析器，用于解析客户端指定的端点

	// 会话粘性：客户端IP+APIKey hash -> 端点名称
	clientEndpointMap map[string]string
	stickyMu          sync.RWMutex
	stickyLastAccess  map[string]time.Time // 记录每个粘性映射的最后访问时间
	stickyExpiry      time.Duration        // 粘性映射过期时间，默认 30 分钟

	// 熔断器：端点名称 -> 熔断器状态
	circuitBreakers  map[string]*CircuitBreaker
	circuitBreakerMu sync.RWMutex

	// 连接排空：端点名称 -> 排空截止时间
	drainingEndpoints map[string]time.Time
	drainMu           sync.RWMutex
	drainTimeout      time.Duration // 排空超时，默认 30 秒

	// 后台任务停止通道
	stopCh chan struct{}
}

// New creates a new Proxy instance
func New(cfg *config.Config, statsStorage StatsStorage, sqliteStorage *storage.SQLiteStorage, deviceID string) *Proxy {
	stats := NewStats(statsStorage, deviceID)

	// Create a reusable HTTP client with connection pool
	// Enhanced configuration for large SSE streaming and HTTP/2 support
	httpClient := &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:           100,
			MaxIdleConnsPerHost:    10,
			IdleConnTimeout:        90 * time.Second,
			TLSHandshakeTimeout:    10 * time.Second,
			ExpectContinueTimeout:  1 * time.Second,
			ResponseHeaderTimeout:  90 * time.Second,
			WriteBufferSize:        128 * 1024, // 128KB write buffer for large SSE streams
			ReadBufferSize:         128 * 1024, // 128KB read buffer for large SSE streams
			MaxResponseHeaderBytes: 64 * 1024,  // 64KB max response headers
		},
	}

	return &Proxy{
		config:            cfg,
		storage:           sqliteStorage,
		stats:             stats,
		currentIndex:      0,
		httpClient:        httpClient,
		activeRequests:    make(map[string]bool),
		endpointCtx:       make(map[string]context.Context),
		endpointCancel:    make(map[string]context.CancelFunc),
		modelsCache:       NewModelsCache(cfg.ModelsCacheTTL),
		resolver:          NewEndpointResolverWithFunc(cfg.GetEndpoints),
		clientEndpointMap: make(map[string]string),
		stickyLastAccess:  make(map[string]time.Time),
		stickyExpiry:      30 * time.Minute,
		circuitBreakers:   make(map[string]*CircuitBreaker),
		drainingEndpoints: make(map[string]time.Time),
		drainTimeout:      30 * time.Second,
		stopCh:            make(chan struct{}),
	}
}

// SetOnEndpointSuccess sets the callback for successful endpoint requests
func (p *Proxy) SetOnEndpointSuccess(callback func(endpointName string)) {
	p.onEndpointSuccess = callback
}

// generateClientID 根据请求生成客户端唯一标识符
// 使用 IP + API Key 的组合哈希，用于会话粘性
func (p *Proxy) generateClientID(r *http.Request, bodyBytes []byte) string {
	var apiKey string
	var clientIP string
	var hashInput string

	// 优先从 Authorization Header 获取 API Key
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && (strings.ToLower(parts[0]) == "bearer" || strings.ToLower(parts[0]) == "apikey") {
			apiKey = parts[1]
		}
	}

	// 其次从 X-API-Key Header 获取
	if apiKey == "" {
		apiKey = r.Header.Get("X-API-Key")
	}

	// 如果都没有，才从请求体中解析（这对于某些 CLI 工具是必要的）
	// 注意：这里只解析 model 字段用于生成 clientID，不再完整解析请求体
	if apiKey == "" && len(bodyBytes) > 0 {
		if idx := bytes.Index(bodyBytes, []byte(`"model"`)); idx >= 0 {
			endIdx := idx + len(`"model"`)
			if endIdx < len(bodyBytes) && bodyBytes[endIdx] == ':' {
				start := endIdx + 1
				for start < len(bodyBytes) && (bodyBytes[start] == ' ' || bodyBytes[start] == '"') {
					start++
				}
				end := start
				for end < len(bodyBytes) && bodyBytes[end] != ',' && bodyBytes[end] != '"' && bodyBytes[end] != '}' {
					end++
				}
				if end > start {
					modelValue := string(bodyBytes[start:end])
					modelValue = strings.TrimSpace(modelValue)
					clientIP = r.RemoteAddr
					if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
						clientIP = strings.Split(forwarded, ",")[0]
					}
					hashInput = fmt.Sprintf("%s|%s|%s", clientIP, apiKey, modelValue)
					return fmt.Sprintf("%x", hashInput)
				}
			}
		}
	}

	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		clientIP = strings.Split(forwarded, ",")[0]
	} else {
		clientIP = r.RemoteAddr
	}

	hashInput = fmt.Sprintf("%s|%s", clientIP, apiKey)
	return fmt.Sprintf("%x", hashInput)
}

// getStickyEndpoint 获取客户端的粘性端点（带过期检查）
func (p *Proxy) getStickyEndpoint(clientID string) string {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()

	// 检查是否过期
	if lastAccess, exists := p.stickyLastAccess[clientID]; exists {
		if time.Since(lastAccess) > p.stickyExpiry {
			// 已过期，删除映射
			delete(p.clientEndpointMap, clientID)
			delete(p.stickyLastAccess, clientID)
			logger.Debug("[Sticky] 客户端 %s 粘性映射已过期", clientID)
			return ""
		}
	}

	endpointName := p.clientEndpointMap[clientID]
	if endpointName != "" {
		// 更新最后访问时间
		p.stickyLastAccess[clientID] = time.Now()
	}
	return endpointName
}

// setStickyEndpoint 设置客户端的粘性端点
func (p *Proxy) setStickyEndpoint(clientID, endpointName string) {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()
	p.clientEndpointMap[clientID] = endpointName
	p.stickyLastAccess[clientID] = time.Now()
	logger.Debug("[Sticky] 客户端 %s 绑定到端点 %s", clientID, endpointName)
}

// clearStickyEndpoint 清除客户端的粘性端点
func (p *Proxy) clearStickyEndpoint(clientID string) {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()
	delete(p.clientEndpointMap, clientID)
	delete(p.stickyLastAccess, clientID)
}

// clearStickyEndpointsForEndpoint 清除所有绑定到指定端点的粘性映射
// 用于当端点不可用时调用
func (p *Proxy) clearStickyEndpointsForEndpoint(endpointName string) {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()

	clearedCount := 0
	for clientID, boundEndpoint := range p.clientEndpointMap {
		if boundEndpoint == endpointName {
			delete(p.clientEndpointMap, clientID)
			delete(p.stickyLastAccess, clientID)
			clearedCount++
		}
	}

	if clearedCount > 0 {
		logger.Info("[Sticky] 端点 %s 不可用，清除了 %d 个粘性映射", endpointName, clearedCount)
	}
}

// clearStickyEndpointsForEndpointExcept 清除所有绑定到指定端点的粘性映射，但保留指定客户端ID的绑定
// 用于端点轮换时，只清除其他客户端的绑定，保留当前正在处理的客户端
func (p *Proxy) clearStickyEndpointsForEndpointExcept(endpointName string, exceptClientID string) {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()

	clearedCount := 0
	for clientID, boundEndpoint := range p.clientEndpointMap {
		if boundEndpoint == endpointName && clientID != exceptClientID {
			delete(p.clientEndpointMap, clientID)
			delete(p.stickyLastAccess, clientID)
			clearedCount++
		}
	}

	if clearedCount > 0 {
		logger.Debug("[Sticky] 端点 %s 不可用，清除了 %d 个其他客户端的粘性映射（保留当前客户端 %s）", endpointName, clearedCount, exceptClientID)
	}
}

// recordStickyEndpoint 记录成功的端点分配（用于会话粘性）
func (p *Proxy) recordStickyEndpoint(clientID, endpointName string) {
	if clientID != "" && endpointName != "" {
		p.setStickyEndpoint(clientID, endpointName)
	}
}

// cleanupExpiredStickyMappings 清理所有过期的粘性映射
// 由后台 goroutine 定期调用
func (p *Proxy) cleanupExpiredStickyMappings() int {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()

	now := time.Now()
	clearedCount := 0

	for clientID, lastAccess := range p.stickyLastAccess {
		if now.Sub(lastAccess) > p.stickyExpiry {
			delete(p.clientEndpointMap, clientID)
			delete(p.stickyLastAccess, clientID)
			clearedCount++
		}
	}

	if clearedCount > 0 {
		logger.Debug("[Sticky] 清理了 %d 个过期的粘性映射", clearedCount)
	}

	return clearedCount
}

// startStickyCleanupRoutine 启动后台清理过期粘性映射的 goroutine
func (p *Proxy) startStickyCleanupRoutine(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-p.stopCh:
				logger.Info("[Sticky] 粘性映射后台清理已停止")
				return
			case <-ticker.C:
				p.cleanupExpiredStickyMappings()
				p.cleanupStaleDrainingEndpoints()
			}
		}
	}()
	logger.Info("[Sticky] 粘性映射后台清理已启动，间隔: %v", interval)
}

// getCircuitBreaker 获取指定端点的熔断器，不存在则创建
func (p *Proxy) getCircuitBreaker(endpointName string) *CircuitBreaker {
	p.circuitBreakerMu.Lock()
	defer p.circuitBreakerMu.Unlock()

	if cb, exists := p.circuitBreakers[endpointName]; exists {
		return cb
	}

	cb := &CircuitBreaker{
		failures:          0,
		networkFailures:   0,
		threshold:         5,  // 连续服务错误(503等) 5 次后打开断路器
		networkThreshold:  10, // 连续网络错误(DNS/连接等) 10 次后打开断路器
		halfOpenSuccesses: 0,
		halfOpenRequired:  2, // 半开状态下需要 2 次成功才能关闭
		openTime:          time.Time{},
		recoveryTimeout:   30 * time.Second, // 30 秒后尝试恢复
		state:             CircuitClosed,
	}
	p.circuitBreakers[endpointName] = cb
	return cb
}

// shouldSkipEndpoint 检查端点是否应该被跳过（断路器开启）
func (p *Proxy) shouldSkipEndpoint(endpointName string) bool {
	cb := p.getCircuitBreaker(endpointName)
	if !cb.ShouldAllow() {
		logger.Debug("[CircuitBreaker] 端点 %s 断路器开启，跳过", endpointName)
		return true
	}
	return false
}

// suggestPossibleAPIPaths 根据 transformer 类型提示可能的 API 路径
func (p *Proxy) suggestPossibleAPIPaths(endpoint config.Endpoint, transformerName string) {
	logger.Warn("[%s] 💡 可能的解决方案:", endpoint.Name)
	logger.Warn("[%s]    当前使用的 Transformer: %s", endpoint.Name, endpoint.Transformer)

	preferredPath := getPreferredAPIPath(endpoint.ProviderType, endpoint.Transformer, endpoint.CustomPath)
	if endpoint.CustomPath != "" {
		logger.Warn("[%s]    ⚠️ 已配置自定义 API 路径: %s (将覆盖自动选择的路径)", endpoint.Name, endpoint.CustomPath)
	}
	logger.Warn("[%s]    根据 ProviderType '%s' 和 Transformer '%s'，推荐的 API 路径是: %s",
		endpoint.Name, endpoint.ProviderType, endpoint.Transformer, preferredPath)

	switch endpoint.Transformer {
	case "openai":
		logger.Warn("[%s]    OpenAI 格式转换器支持以下 API 路径:", endpoint.Name)
		logger.Warn("[%s]      - /v1/chat/completions (标准 OpenAI Chat API)", endpoint.Name)
		logger.Warn("[%s]      - /v1/responses (OpenAI Responses API)", endpoint.Name)
		logger.Warn("[%s]    提示: 尝试将 Transformer 改为 'openai2' 如果中转站使用 Responses API", endpoint.Name)
	case "openai2":
		logger.Warn("[%s]    OpenAI2 格式转换器支持以下 API 路径:", endpoint.Name)
		logger.Warn("[%s]      - /v1/responses (OpenAI Responses API)", endpoint.Name)
		logger.Warn("[%s]    提示: 某些中转站可能只支持 /v1/chat/completions", endpoint.Name)
		logger.Warn("[%s]    提示: 尝试将 Transformer 改为 'openai' 如果中转站使用 Chat API", endpoint.Name)
	case "passthrough", "claude":
		logger.Warn("[%s]    Claude 格式转换器支持以下 API 路径:", endpoint.Name)
		logger.Warn("[%s]      - /v1/messages (Claude Messages API)", endpoint.Name)
		logger.Warn("[%s]    提示: 确认中转站是否支持 Claude Messages API", endpoint.Name)
	case "gemini":
		logger.Warn("[%s]    Gemini 格式转换器支持以下 API 路径:", endpoint.Name)
		logger.Warn("[%s]      - /v1beta/models/{model}:generateContent", endpoint.Name)
		logger.Warn("[%s]      - /v1beta/models/{model}:streamGenerateContent (流式)", endpoint.Name)
		logger.Warn("[%s]    提示: 确认中转站的 Gemini API 路径是否正确", endpoint.Name)
	default:
		logger.Warn("[%s]    未知 Transformer 类型: %s", endpoint.Name, endpoint.Transformer)
		logger.Warn("[%s]    支持的 Transformer 类型: passthrough, openai, openai2, gemini", endpoint.Name)
	}

	logger.Warn("[%s] 💡 ProviderType (中转方案) 说明:", endpoint.Name)
	logger.Warn("[%s]    - oneapi/newapi: One API / New API 兼容方案", endpoint.Name)
	logger.Warn("[%s]    - sub2api: Sub2API 方案", endpoint.Name)
	logger.Warn("[%s]    - cliproxyapi: CLIProxyAPI 方案", endpoint.Name)
	logger.Warn("[%s]    - native: 原生 API (不做转换)", endpoint.Name)

	logger.Warn("[%s] 💡 通用建议:", endpoint.Name)
	if endpoint.ProviderType != "" {
		logger.Warn("[%s]    已配置 ProviderType: %s", endpoint.Name, endpoint.ProviderType)
	}
	logger.Warn("[%s]    1. 确认中转站的 API 文档，了解其支持的 API 格式", endpoint.Name)
	logger.Warn("[%s]    2. 检查中转站的模型列表，确保所需的模型可用", endpoint.Name)
	logger.Warn("[%s]    3. 某些中转站可能需要特定的身份验证方式", endpoint.Name)
	logger.Warn("[%s]    4. 尝试访问中转站的 /v1/models 端点查看支持的模型", endpoint.Name)
}

// recordEndpointSuccess 记录端点成功
func (p *Proxy) recordEndpointSuccess(endpointName string) {
	cb := p.getCircuitBreaker(endpointName)
	cb.RecordSuccess()
}

// recordEndpointFailure 记录端点失败
func (p *Proxy) recordEndpointFailure(endpointName string, err error) {
	cb := p.getCircuitBreaker(endpointName)
	cb.RecordFailure(IsNetworkError(err))
}

// resetCircuitBreaker 重置端点的熔断器状态
func (p *Proxy) resetCircuitBreaker(endpointName string) {
	p.circuitBreakerMu.Lock()
	defer p.circuitBreakerMu.Unlock()

	if cb, exists := p.circuitBreakers[endpointName]; exists {
		cb.mu.Lock()
		cb.failures = 0
		cb.state = CircuitClosed
		cb.halfOpenSuccesses = 0
		cb.mu.Unlock()
		logger.Info("[CircuitBreaker] 端点 %s 熔断器已重置", endpointName)
	}
}

// clearCircuitBreaker 清除端点的熔断器
func (p *Proxy) clearCircuitBreaker(endpointName string) {
	p.circuitBreakerMu.Lock()
	defer p.circuitBreakerMu.Unlock()

	delete(p.circuitBreakers, endpointName)
	logger.Debug("[CircuitBreaker] 端点 %s 熔断器已清除", endpointName)
}

// getPreferredAPIPath 根据 ProviderType、Transformer 和 CustomPath 返回最佳 API 路径
func getPreferredAPIPath(providerType, transformer, customPath string) string {
	if customPath != "" {
		return customPath
	}

	providerType = strings.ToLower(providerType)
	transformer = strings.ToLower(transformer)

	switch providerType {
	case "oneapi", "newapi":
		switch transformer {
		case "openai", "passthrough", "claude", "auto":
			return "/v1/chat/completions"
		case "openai2":
			return "/v1/responses"
		case "gemini":
			return "/v1beta/models/{model}:generateContent"
		default:
			return "/v1/chat/completions"
		}
	case "sub2api":
		switch transformer {
		case "openai", "passthrough", "claude", "auto":
			return "/v1/chat/completions"
		case "openai2":
			return "/v1/responses"
		case "gemini":
			return "/v1beta/models/{model}:generateContent"
		default:
			return "/v1/chat/completions"
		}
	case "cliproxyapi":
		switch transformer {
		case "openai", "auto":
			return "/v1/chat/completions"
		case "openai2":
			return "/v1/responses"
		case "passthrough", "claude":
			return "/v1/messages"
		case "gemini":
			return "/v1beta/models/{model}:generateContent"
		default:
			return "/v1/chat/completions"
		}
	case "native", "":
		switch transformer {
		case "openai", "auto":
			return "/v1/chat/completions"
		case "openai2":
			return "/v1/responses"
		case "passthrough", "claude":
			return "/v1/messages"
		case "gemini":
			return "/v1beta/models/{model}:generateContent"
		default:
			return "/v1/chat/completions"
		}
	default:
		switch transformer {
		case "openai", "auto":
			return "/v1/chat/completions"
		case "openai2":
			return "/v1/responses"
		case "passthrough", "claude":
			return "/v1/messages"
		case "gemini":
			return "/v1beta/models/{model}:generateContent"
		default:
			return "/v1/chat/completions"
		}
	}
}

// probeAvailablePaths 探测端点支持的 API 路径
// 如果 ProviderType 表明是第三方中转站，则跳过探测以避免触发限流
func (p *Proxy) probeAvailablePaths(endpoint config.Endpoint, modelName string) []string {
	providerType := strings.ToLower(strings.TrimSpace(endpoint.ProviderType))

	// 对于第三方中转站，跳过探测，直接使用推断的路径
	if shouldSkipProbe(providerType) {
		inferredPath := inferAPIPathFromConfig(providerType, endpoint.Transformer)
		logger.Debug("[%s] 跳过探测 (第三方中转站), 使用推断路径: %s", endpoint.Name, inferredPath)
		return []string{inferredPath}
	}

	var availablePaths []string
	pathsToProbe := []string{
		"/v1/chat/completions",
		"/v1/responses",
		"/v1/messages",
		"/v1beta/models/" + modelName + ":generateContent",
	}

	client := &http.Client{Timeout: 5 * time.Second}
	for i, path := range pathsToProbe {
		url := strings.TrimSuffix(endpoint.APIUrl, "/") + path
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			// 探测失败时，等待一下再尝试下一个，避免触发限流
			if i < len(pathsToProbe)-1 {
				time.Sleep(500 * time.Millisecond)
			}
			continue
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusUnauthorized {
			availablePaths = append(availablePaths, path)
			logger.Debug("[%s] 探测到可用路径: %s (状态码: %d)", endpoint.Name, path, resp.StatusCode)
		}

		// 每个探测之间添加延迟，避免触发限流
		if i < len(pathsToProbe)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	if len(availablePaths) == 0 {
		// 探测失败时，回退到推断的路径
		inferredPath := inferAPIPathFromConfig(providerType, endpoint.Transformer)
		logger.Warn("[%s] 无法探测到可用的 API 路径，回退到推断路径: %s", endpoint.Name, inferredPath)
		return []string{inferredPath}
	}
	return availablePaths
}

// Start starts the proxy server
func (p *Proxy) Start() error {
	return p.StartWithMux(nil)
}

// StartWithMux starts the proxy server with an optional custom mux
func (p *Proxy) StartWithMux(customMux *http.ServeMux) error {
	// 启动粘性映射后台清理任务（每 5 分钟清理一次）
	p.startStickyCleanupRoutine(5 * time.Minute)

	port := p.config.GetPort()

	var mux *http.ServeMux
	if customMux != nil {
		mux = customMux
	} else {
		mux = http.NewServeMux()
	}

	// Register proxy routes
	mux.HandleFunc("/", p.handleProxy)
	mux.HandleFunc("/v1/messages/count_tokens", p.handleCountTokens)
	mux.HandleFunc("/v1/models", p.handleModels)
	mux.HandleFunc("/health", p.handleHealth)
	mux.HandleFunc("/stats", p.handleStats)

	p.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	logger.Info("ccg starting on port %d", port)
	logger.Info("Configured %d endpoints", len(p.config.GetEndpoints()))

	return p.server.ListenAndServe()
}

// Stop stops the proxy server and all background goroutines
func (p *Proxy) Stop() error {
	close(p.stopCh)
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}

// getEnabledEndpoints returns only the enabled endpoints
func (p *Proxy) getEnabledEndpoints() []config.Endpoint {
	allEndpoints := p.config.GetEndpoints()
	enabled := make([]config.Endpoint, 0)
	for _, ep := range allEndpoints {
		if ep.Enabled {
			enabled = append(enabled, ep)
		}
	}
	return enabled
}

// getCurrentEndpoint returns the current endpoint (thread-safe)
func (p *Proxy) getCurrentEndpoint() config.Endpoint {
	p.mu.RLock()
	defer p.mu.RUnlock()

	endpoints := p.getEnabledEndpoints()
	if len(endpoints) == 0 {
		// Return empty endpoint if no enabled endpoints
		return config.Endpoint{}
	}
	// Make sure currentIndex is within bounds
	index := p.currentIndex % len(endpoints)
	return endpoints[index]
}

// markRequestActive marks an endpoint as having active requests
func (p *Proxy) markRequestActive(endpointName string) {
	p.activeRequestsMu.Lock()
	defer p.activeRequestsMu.Unlock()
	p.activeRequests[endpointName] = true
}

// markRequestInactive marks an endpoint as having no active requests
func (p *Proxy) markRequestInactive(endpointName string) {
	p.activeRequestsMu.Lock()
	defer p.activeRequestsMu.Unlock()
	delete(p.activeRequests, endpointName)
}

// hasActiveRequests checks if an endpoint has active requests
func (p *Proxy) hasActiveRequests(endpointName string) bool {
	p.activeRequestsMu.RLock()
	defer p.activeRequestsMu.RUnlock()
	return p.activeRequests[endpointName]
}

// isCurrentEndpoint checks if the given endpoint is still the current one
func (p *Proxy) isCurrentEndpoint(endpointName string) bool {
	current := p.getCurrentEndpoint()
	return current.Name == endpointName
}

// getEndpointContext returns a context for the given endpoint, creating one if needed
func (p *Proxy) getEndpointContext(endpointName string) context.Context {
	p.ctxMu.Lock()
	defer p.ctxMu.Unlock()

	if ctx, ok := p.endpointCtx[endpointName]; ok {
		return ctx
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.endpointCtx[endpointName] = ctx
	p.endpointCancel[endpointName] = cancel
	return ctx
}

// cancelEndpointRequests cancels all requests for the given endpoint
func (p *Proxy) cancelEndpointRequests(endpointName string) {
	p.ctxMu.Lock()
	defer p.ctxMu.Unlock()

	if cancel, ok := p.endpointCancel[endpointName]; ok {
		cancel()
		delete(p.endpointCtx, endpointName)
		delete(p.endpointCancel, endpointName)
	}
}

// rotateEndpoint switches to the next endpoint (thread-safe)
// Uses connection draining to gracefully handle ongoing requests on the old endpoint
// Skips endpoints with circuit breakers open
func (p *Proxy) rotateEndpoint() config.Endpoint {
	oldEndpoint := p.getCurrentEndpoint()

	// 清除绑定到旧端点的所有粘性映射（保留当前客户端的绑定）
	p.clearStickyEndpointsForEndpointExcept(oldEndpoint.Name, "")

	// Now acquire lock and perform the rotation
	p.mu.Lock()
	defer p.mu.Unlock()

	endpoints := p.getEnabledEndpoints()
	if len(endpoints) == 0 {
		return config.Endpoint{}
	}

	// Find next available endpoint (skip circuit breakers and draining endpoints)
	maxAttempts := len(endpoints)
	var validCandidate config.Endpoint
	var validIndex int
	found := false

	for i := 0; i < maxAttempts; i++ {
		nextIndex := (p.currentIndex + 1 + i) % len(endpoints)
		candidate := endpoints[nextIndex]

		cb := p.getCircuitBreaker(candidate.Name)
		if cb.ShouldAllow() && !p.isDrainingLocked(candidate.Name) {
			validCandidate = candidate
			validIndex = nextIndex
			found = true
			break
		}
		logger.Debug("[SWITCH] 跳过端点 %s (断路器开启或正在排空)", candidate.Name)
	}

	if found {
		// Only drain old endpoint and update index when we actually found a valid candidate
		if len(endpoints) > 1 && oldEndpoint.Name != validCandidate.Name {
			p.startDrainingEndpoint(oldEndpoint.Name)
			logger.Debug("[SWITCH] %s → %s (#%d) (旧端点开始排空)", oldEndpoint.Name, validCandidate.Name, validIndex+1)
		}
		p.currentIndex = validIndex
		return validCandidate
	}

	// No available endpoints found
	logger.Warn("[SWITCH] 所有可用端点均不可用（断路器开启或正在排空）")
	return config.Endpoint{}
}

// isDrainingLocked checks if an endpoint is draining (called with lock held)
func (p *Proxy) isDrainingLocked(endpointName string) bool {
	deadline, exists := p.drainingEndpoints[endpointName]
	if !exists {
		return false
	}
	return time.Now().Before(deadline)
}

// GetCurrentEndpointName returns the current endpoint name (thread-safe)
func (p *Proxy) GetCurrentEndpointName() string {
	endpoint := p.getCurrentEndpoint()
	return endpoint.Name
}

// SetCurrentEndpoint manually switches to a specific endpoint by name
// Returns error if endpoint not found or not enabled
// Thread-safe and gracefully drains the old endpoint before switching
func (p *Proxy) SetCurrentEndpoint(targetName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	endpoints := p.getEnabledEndpoints()
	if len(endpoints) == 0 {
		return fmt.Errorf("no enabled endpoints")
	}

	// Find the endpoint by name
	for i, ep := range endpoints {
		if ep.Name == targetName {
			oldEndpoint := endpoints[p.currentIndex%len(endpoints)]
			if oldEndpoint.Name != targetName {
				// 开始排空旧端点，而不是立即取消请求
				// 这样可以让正在进行的请求继续完成，同时新请求会路由到新端点
				p.startDrainingEndpoint(oldEndpoint.Name)
				// 重置旧端点的熔断器，使其可以重新尝试
				p.resetCircuitBreaker(oldEndpoint.Name)
			}
			p.currentIndex = i
			logger.Info("[MANUAL SWITCH] %s → %s (旧端点开始排空)", oldEndpoint.Name, ep.Name)
			return nil
		}
	}

	return fmt.Errorf("endpoint '%s' not found or not enabled", targetName)
}

// ClientFormat represents the API format used by the client
type ClientFormat string

const (
	ClientFormatClaude          ClientFormat = "claude"           // Claude Code: /v1/messages
	ClientFormatOpenAIChat      ClientFormat = "openai_chat"      // Codex (chat): /v1/chat/completions
	ClientFormatOpenAIResponses ClientFormat = "openai_responses" // Codex (responses): /v1/responses
)

// detectClientFormat identifies the client format based on request path
func detectClientFormat(path string) ClientFormat {
	switch {
	case strings.HasPrefix(path, "/v1/chat/completions") || strings.HasPrefix(path, "/chat/completions"):
		return ClientFormatOpenAIChat
	case strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/responses"):
		return ClientFormatOpenAIResponses
	default:
		return ClientFormatClaude
	}
}

// handleProxy handles the main proxy logic
func (p *Proxy) handleProxy(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Error("Failed to read request body: %v", err)
		p.writeJSONError(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	requestStart := time.Now()
	reqBytes := len(bodyBytes)

	// Detect client format
	clientFormat := detectClientFormat(r.URL.Path)

	logger.DebugLog("=== Proxy Request ===")
	logger.DebugLog("Method: %s, Path: %s, ClientFormat: %s", r.Method, r.URL.Path, clientFormat)
	logger.DebugLog("Request Body: %s", string(bodyBytes))

	var streamReq struct {
		Model    string      `json:"model"`
		Thinking interface{} `json:"thinking"`
		Stream   bool        `json:"stream"`
	}
	json.Unmarshal(bodyBytes, &streamReq)

	// 在解析时记录原始模型名称，用于后续处理
	// originalModelName := strings.TrimSpace(streamReq.Model)

	endpoints := p.getEnabledEndpoints()
	if len(endpoints) == 0 {
		logger.Error("No enabled endpoints available")
		p.writeJSONError(w, "No enabled endpoints configured", http.StatusServiceUnavailable)
		return
	}

	// 尝试解析客户端指定的端点
	specifiedEndpoint, modelOverride, resolveErr := p.resolver.ResolveEndpoint(r, bodyBytes)
	if resolveErr != nil {
		// 端点指定错误，返回错误响应
		logger.Warn("端点解析失败: %v", resolveErr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		errorResp := map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "invalid_request_error",
				"message": resolveErr.Error(),
			},
		}
		if jsonBytes, err := json.Marshal(errorResp); err == nil {
			w.Write(jsonBytes)
		}
		return
	}

	// 如果指定了端点，使用该端点；否则使用轮询机制
	var useSpecificEndpoint bool
	if specifiedEndpoint != nil {
		useSpecificEndpoint = true
		logger.Debug("[Resolver] 使用指定端点: %s", specifiedEndpoint.Name)
	}

	// 生成客户端标识符（用于会话粘性）
	clientID := p.generateClientID(r, bodyBytes)

	maxRetries := p.computeMaxRetries(endpoints)
	endpointAttempts := 0
	lastEndpointName := ""
	refreshedCredentialAttempts := make(map[int64]bool)
	fallbackModel := ""
	fallbackOriginalModel := ""
	hasTriedFallback := false

	// 会话粘性：检查是否有已分配的端点
	stickyEndpointName := ""
	if !useSpecificEndpoint {
		stickyEndpointName = p.getStickyEndpoint(clientID)
		if stickyEndpointName != "" {
			logger.Debug("[Sticky] 客户端 %s 使用粘性端点: %s", clientID, stickyEndpointName)
		}
	}

	for retry := 0; retry < maxRetries; retry++ {
		var endpoint config.Endpoint
		if useSpecificEndpoint {
			// 使用指定的端点，不进行轮询
			endpoint = *specifiedEndpoint
		} else if stickyEndpointName != "" {
			// 使用粘性端点（如果仍然可用且未在排空）
			found := false
			for _, ep := range endpoints {
				if ep.Name == stickyEndpointName {
					// 检查端点是否在排空
					if p.isDraining(ep.Name) {
						logger.Warn("[Sticky] 粘性端点 %s 正在排空，切换到轮询", stickyEndpointName)
						p.clearStickyEndpoint(clientID)
						stickyEndpointName = ""
						endpoint = p.getCurrentEndpoint()
					} else {
						endpoint = ep
						found = true
					}
					break
				}
			}
			if !found && stickyEndpointName != "" {
				// 粘性端点已不可用，清除并使用轮询
				// 保留当前客户端的绑定（会在下次请求时自然失效），只清除其他客户端
				logger.Warn("[Sticky] 粘性端点 %s 不可用，切换到轮询", stickyEndpointName)
				p.clearStickyEndpoint(clientID)
				p.clearStickyEndpointsForEndpointExcept(stickyEndpointName, clientID)
				stickyEndpointName = ""
				endpoint = p.getCurrentEndpoint()
			}
		} else {
			// 使用轮询机制
			endpoint = p.getCurrentEndpoint()
		}

		if endpoint.Name == "" {
			p.writeJSONError(w, "No enabled endpoints available", http.StatusServiceUnavailable)
			return
		}

		// 熔断器检查：如果端点的断路器开启，跳过该端点
		if p.shouldSkipEndpoint(endpoint.Name) {
			logger.Warn("[CircuitBreaker] 端点 %s 断路器开启，跳过", endpoint.Name)
			p.markRequestInactive(endpoint.Name)
			if useSpecificEndpoint {
				p.writeJSONError(w, fmt.Sprintf("Endpoint %s is unavailable (circuit breaker open)", endpoint.Name), http.StatusServiceUnavailable)
				return
			}
			newEndpoint := p.rotateEndpoint()
			if newEndpoint.Name == "" {
				logger.Error("[CircuitBreaker] 所有端点断路器均开启，无法处理请求")
				p.writeJSONError(w, "All endpoints are unavailable due to circuit breakers", http.StatusServiceUnavailable)
				return
			}
			continue
		}

		// 排空检查：如果端点正在排空，跳过该端点
		if p.isDraining(endpoint.Name) {
			logger.Warn("[Drain] 端点 %s 正在排空，跳过", endpoint.Name)
			p.markRequestInactive(endpoint.Name)
			if useSpecificEndpoint {
				p.writeJSONError(w, fmt.Sprintf("Endpoint %s is draining and unavailable", endpoint.Name), http.StatusServiceUnavailable)
				return
			}
			newEndpoint := p.rotateEndpoint()
			if newEndpoint.Name == "" {
				logger.Error("[Drain] 所有端点均正在排空，无法处理请求")
				p.writeJSONError(w, "All endpoints are draining and unavailable", http.StatusServiceUnavailable)
				return
			}
			continue
		}

		// Reset attempts counter if endpoint changed (e.g., manual switch)
		if lastEndpointName != "" && lastEndpointName != endpoint.Name {
			endpointAttempts = 0
		}
		lastEndpointName = endpoint.Name

		endpointAttempts++
		p.markRequestActive(endpoint.Name)

		authMode := config.NormalizeAuthMode(endpoint.AuthMode)
		apiKey := strings.TrimSpace(endpoint.APIKey)
		credentialID := int64(0)
		var selectedCredential *storage.EndpointCredential
		if config.IsTokenPoolAuthMode(authMode) {
			credential, err := p.selectCredential(endpoint.Name)
			if err != nil {
				logger.Warn("[%s] Failed to select token pool credential: %v", endpoint.Name, err)
				p.stats.RecordError(endpoint.Name)
				p.markRequestInactive(endpoint.Name)
				if endpointAttempts >= 2 && !useSpecificEndpoint {
					p.rotateEndpoint()
					endpointAttempts = 0
				}
				continue
			}
			if credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
				logger.Warn("[%s] No usable token in token pool", endpoint.Name)
				p.stats.RecordError(endpoint.Name)
				p.markRequestInactive(endpoint.Name)
				if endpointAttempts >= 2 && !useSpecificEndpoint {
					p.rotateEndpoint()
					endpointAttempts = 0
				}
				continue
			}
			selectedCredential = credential
			if shouldTryCredentialRefresh(credential, time.Now().UTC()) {
				refreshed, refreshErr := p.refreshCredential(endpoint, credential)
				if refreshErr != nil {
					logger.Warn("[%s] Preflight credential refresh failed (id=%d): %v", endpoint.Name, credential.ID, refreshErr)
				} else {
					selectedCredential = refreshed
					refreshedCredentialAttempts[refreshed.ID] = true
				}
			}
			apiKey = strings.TrimSpace(credential.AccessToken)
			if selectedCredential != nil {
				apiKey = strings.TrimSpace(selectedCredential.AccessToken)
				credentialID = selectedCredential.ID
			}
		} else if apiKey == "" {
			logger.Warn("[%s] API key mode but apiKey is empty", endpoint.Name)
			p.stats.RecordError(endpoint.Name)
			p.markRequestInactive(endpoint.Name)
			if endpointAttempts >= 2 {
				p.rotateEndpoint()
				endpointAttempts = 0
			}
			continue
		}

		trans, err := prepareTransformerForClient(clientFormat, endpoint)
		if err != nil {
			logger.Error("[%s] %v", endpoint.Name, err)
			p.stats.RecordError(endpoint.Name)
			p.markRequestInactive(endpoint.Name)
			if endpointAttempts >= 2 {
				p.rotateEndpoint()
				endpointAttempts = 0
			}
			continue
		}

		transformerName := trans.Name()
		logger.Debug("[%s] 转换器选择: clientFormat=%s, endpointTransformer=%s → %s", endpoint.Name, clientFormat, endpoint.Transformer, transformerName)

		transformedBody, err := trans.TransformRequest(bodyBytes)
		if err != nil {
			logger.Error("[%s] Failed to transform request: %v", endpoint.Name, err)
			p.stats.RecordError(endpoint.Name)
			p.markRequestInactive(endpoint.Name)
			if endpointAttempts >= 2 {
				p.rotateEndpoint()
				endpointAttempts = 0
			}
			continue
		}

		logger.DebugLog("[%s] Transformer: %s", endpoint.Name, transformerName)
		logger.DebugLog("[%s] Transformed Request: %s", endpoint.Name, string(transformedBody))

		// 如果有模型覆盖值，应用到转换后的请求体中
		if modelOverride != "" {
			transformedBody = overrideModelInPayload(transformedBody, modelOverride)
			logger.DebugLog("[%s] 应用模型覆盖后的请求: %s", endpoint.Name, string(transformedBody))
		}

		// 如果有 fallback 模型（模型不支持时的降级），应用到转换后的请求体中
		if fallbackModel != "" && config.NormalizeAuthMode(authMode) != config.AuthModeCodexTokenPool {
			transformedBody = overrideModelInPayload(transformedBody, fallbackModel)
			logger.DebugLog("[%s] 应用 fallback 模型后的请求: %s", endpoint.Name, string(transformedBody))
		}

		cleanedBody, err := cleanIncompleteToolCalls(transformedBody)
		if err != nil {
			logger.Warn("[%s] Failed to clean tool calls: %v", endpoint.Name, err)
			cleanedBody = transformedBody
		}
		transformedBody = cleanedBody

		modelName := strings.TrimSpace(streamReq.Model)
		originalModelName := modelName

		if modelOverride != "" {
			modelName = modelOverride
			logger.Debug("[%s] 使用模型覆盖值: %s", endpoint.Name, modelName)
		} else if fallbackModel != "" {
			modelName = fallbackModel
			logger.Debug("[%s] 使用 fallback 模型: %s", endpoint.Name, modelName)
		} else if len(endpoint.ModelMappings) > 0 {
			if mappedModel, ok := endpoint.ModelMappings[modelName]; ok {
				modelName = mappedModel
				logger.Debug("[%s] 模型名称映射: %s → %s (根据端点模型映射配置)", endpoint.Name, originalModelName, modelName)
			} else if endpoint.Model != "" {
				modelName = endpoint.Model
				logger.Debug("[%s] 动态模型映射: %s → %s (根据端点配置自动替换)", endpoint.Name, originalModelName, modelName)
			} else if modelName == "" && !useSpecificEndpoint {
				if autoModel, availableModels := p.GetFirstAvailableModel(endpoint.Name); autoModel != "" {
					modelName = autoModel
					logger.Debug("[%s] 自动发现模型: %s (可用模型数: %d)", endpoint.Name, modelName, len(availableModels))
				}
			}
		} else if endpoint.Model != "" && modelName != endpoint.Model {
			modelName = endpoint.Model
			logger.Debug("[%s] 动态模型映射: %s → %s (根据端点配置自动替换)", endpoint.Name, originalModelName, modelName)
		} else if modelName == "" {
			modelName = endpoint.Model
			if modelName == "" && !useSpecificEndpoint {
				if autoModel, availableModels := p.GetFirstAvailableModel(endpoint.Name); autoModel != "" {
					modelName = autoModel
					logger.Debug("[%s] 自动发现模型: %s (可用模型数: %d)", endpoint.Name, modelName, len(availableModels))
				}
			}
		}

		autoSwitched := false
		if valid, availModels := p.ValidateModelForEndpoint(endpoint.Name, modelName); !valid && availModels != nil && len(availModels) > 0 {
			if !useSpecificEndpoint {
				logger.Warn("[%s] 模型 %s 不在 API Key 权限范围内，自动切换到: %s", endpoint.Name, modelName, availModels[0])
				modelName = availModels[0]
				autoSwitched = true
			} else {
				if len(availModels) <= 10 {
					logger.Warn("[%s] 模型 %s 不在 API Key 权限范围内，可用模型: %v", endpoint.Name, modelName, availModels)
				} else {
					logger.Warn("[%s] 模型 %s 不在 API Key 权限范围内，可用模型(前10个): %v", endpoint.Name, modelName, availModels[:10])
				}
			}
		} else if !valid && modelName != "" {
			logger.Debug("[%s] 模型缓存未初始化，跳过模型验证", endpoint.Name)
		}

		if modelName != originalModelName {
			transformedBody = overrideModelInPayload(transformedBody, modelName)
			var reason string
			switch {
			case authMode == config.AuthModeCodexTokenPool:
				reason = "CodexTokenPool"
			case modelOverride != "":
				reason = "模型覆盖"
			case fallbackModel != "":
				reason = "fallback"
			case len(endpoint.ModelMappings) > 0 && originalModelName != modelName:
				reason = "模型映射"
			case endpoint.Model != "" && originalModelName != modelName:
				reason = "动态映射"
			case autoSwitched:
				reason = "自动切换"
			default:
				reason = "自动发现"
			}
			logger.Debug("[%s] 替换请求体中的模型(%s): %s → %s", endpoint.Name, reason, originalModelName, modelName)
		}

		var thinkingEnabled bool
		if strings.Contains(transformerName, "openai") {
			var openaiReq map[string]interface{}
			if err := json.Unmarshal(transformedBody, &openaiReq); err == nil {
				if enable, ok := openaiReq["enable_thinking"].(bool); ok {
					thinkingEnabled = enable
				}
			}
		}

		proxyReq, err := buildProxyRequest(r, endpoint, apiKey, transformedBody, transformerName, selectedCredential)
		if err != nil {
			logger.Error("[%s] Failed to create request: %v", endpoint.Name, err)
			p.stats.RecordError(endpoint.Name)
			p.markRequestInactive(endpoint.Name)
			if endpointAttempts >= 2 {
				p.rotateEndpoint()
				endpointAttempts = 0
			}
			continue
		}

		proxyURL := resolveProxyURLForRequest(p.config, proxyReq.URL)
		proxyLabel := strings.TrimSpace(proxyURL)
		if streamReq.Stream {
			if proxyLabel == "" {
				logger.Debug("[%s] Streaming %s %d", endpoint.Name, modelName, reqBytes)
			} else {
				logger.Debug("[%s] Streaming %s %d %s", endpoint.Name, modelName, reqBytes, proxyLabel)
			}
		} else {
			if proxyLabel == "" {
				logger.Debug("[%s] Requesting %s %d", endpoint.Name, modelName, reqBytes)
			} else {
				logger.Debug("[%s] Requesting %s %d %s", endpoint.Name, modelName, reqBytes, proxyLabel)
			}
		}

		ctx := p.getEndpointContext(endpoint.Name)

		logger.Debug("[%s] >>> REQUEST START >>>", endpoint.Name)
		logger.Debug("[%s] URL: %s %s", endpoint.Name, proxyReq.Method, proxyReq.URL.String())
		logger.Debug("[%s] Headers: %v", endpoint.Name, proxyReq.Header)
		bodyPreview := string(transformedBody)
		if len(bodyPreview) > 500 {
			bodyPreview = bodyPreview[:500] + "...(truncated)"
		}
		logger.Debug("[%s] Body: %s", endpoint.Name, bodyPreview)
		logger.Debug("[%s] >>> REQUEST END >>>", endpoint.Name)

		resp, err := sendRequest(ctx, proxyReq, p.httpClient, p.config)
		if err != nil {
			logger.Error("[%s] Request failed: %v", endpoint.Name, err)
			if isTransientNetworkError(err) {
				logger.Warn("[%s] Transient network error, retrying same endpoint: %v", endpoint.Name, err)
				p.markRequestInactive(endpoint.Name)
				time.Sleep(300 * time.Millisecond)
				endpointAttempts = 0
				continue
			}
			p.markCredentialFailure(credentialID, 0, err.Error())
			p.recordCredentialUsage(credentialID, endpoint.Name, 0, 1, 0, 0)
			p.stats.RecordError(endpoint.Name)
			p.recordEndpointFailure(endpoint.Name, err)
			p.markRequestInactive(endpoint.Name)
			if endpointAttempts >= 2 {
				p.rotateEndpoint()
				endpointAttempts = 0
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			p.captureCodexRateLimitsFromHeaders(endpoint, credentialID, resp.Header)
		}

		contentType := resp.Header.Get("Content-Type")
		isStreaming := shouldHandleAsStreamingResponse(contentType, streamReq.Stream, endpoint, transformerName)

		// Codex backend enforces stream=true upstream for /responses in some environments.
		// Bridge to non-stream client responses regardless of upstream Content-Type quirks.
		if resp.StatusCode == http.StatusOK && !streamReq.Stream && shouldAggregateCodexStreaming(endpoint, transformerName) {
			inputTokens, outputTokens, outputText, err := p.handleStreamingAsNonStreaming(w, resp, endpoint, trans, credentialID)
			if err == nil {
				// Fallback: estimate tokens when usage is missing.
				if inputTokens == 0 || outputTokens == 0 {
					inputTokens, outputTokens = p.estimateTokens(bodyBytes, outputText, inputTokens, outputTokens, endpoint.Name)
				}

				p.stats.RecordRequest(endpoint.Name)
				p.stats.RecordTokens(endpoint.Name, inputTokens, outputTokens)
				p.recordCredentialUsage(credentialID, endpoint.Name, 1, 0, inputTokens, outputTokens)
				p.markCredentialSuccess(credentialID)
				p.markRequestInactive(endpoint.Name)
				if p.onEndpointSuccess != nil {
					p.onEndpointSuccess(endpoint.Name)
				}
				p.recordStickyEndpoint(clientID, endpoint.Name)
				totalElapsed := time.Since(requestStart).Round(time.Millisecond)
				logger.Debug("[%s] Requested tokens=%d/%d latency=%s cred_id=%d", endpoint.Name, inputTokens, outputTokens, totalElapsed, credentialID)
				return
			}
			logger.Warn("[%s] Failed to aggregate streaming response as non-stream: %v", endpoint.Name, err)
			p.markCredentialFailure(credentialID, 0, err.Error())
			p.recordCredentialUsage(credentialID, endpoint.Name, 0, 1, 0, 0)
			p.stats.RecordError(endpoint.Name)
			p.markRequestInactive(endpoint.Name)
			if endpointAttempts >= 2 {
				p.rotateEndpoint()
				endpointAttempts = 0
			}
			continue
		}

		if resp.StatusCode == http.StatusOK && isStreaming {
			inputTokens, outputTokens, outputText := p.handleStreamingResponse(w, resp, endpoint, trans, transformerName, thinkingEnabled, modelName, bodyBytes, credentialID)

			// Fallback: estimate tokens when usage is 0
			if inputTokens == 0 || outputTokens == 0 {
				inputTokens, outputTokens = p.estimateTokens(bodyBytes, outputText, inputTokens, outputTokens, endpoint.Name)
			}

			p.stats.RecordRequest(endpoint.Name)
			p.stats.RecordTokens(endpoint.Name, inputTokens, outputTokens)
			p.recordCredentialUsage(credentialID, endpoint.Name, 1, 0, inputTokens, outputTokens)
			p.markCredentialSuccess(credentialID)
			p.markRequestInactive(endpoint.Name)
			p.recordEndpointSuccess(endpoint.Name)
			if p.onEndpointSuccess != nil {
				p.onEndpointSuccess(endpoint.Name)
			}
			p.recordStickyEndpoint(clientID, endpoint.Name)
			totalElapsed := time.Since(requestStart).Round(time.Millisecond)
			logger.Debug("[%s] Requested tokens=%d/%d latency=%s cred_id=%d", endpoint.Name, inputTokens, outputTokens, totalElapsed, credentialID)
			return
		}

		if resp.StatusCode == http.StatusOK {
			inputTokens, outputTokens, err := p.handleNonStreamingResponse(w, resp, endpoint, trans)
			if err == nil {
				p.stats.RecordRequest(endpoint.Name)
				p.stats.RecordTokens(endpoint.Name, inputTokens, outputTokens)
				p.recordCredentialUsage(credentialID, endpoint.Name, 1, 0, inputTokens, outputTokens)
				p.markCredentialSuccess(credentialID)
				p.markRequestInactive(endpoint.Name)
				p.recordEndpointSuccess(endpoint.Name)
				if p.onEndpointSuccess != nil {
					p.onEndpointSuccess(endpoint.Name)
				}
				p.recordStickyEndpoint(clientID, endpoint.Name)
				totalElapsed := time.Since(requestStart).Round(time.Millisecond)
				logger.Debug("[%s] Requested tokens=%d/%d latency=%s cred_id=%d", endpoint.Name, inputTokens, outputTokens, totalElapsed, credentialID)
				return
			}
		}

		if shouldRetry(resp.StatusCode) {
			var errBody []byte
			if resp.Header.Get("Content-Encoding") == "gzip" {
				errBody, _ = decompressGzip(resp.Body)
			} else {
				errBody, _ = io.ReadAll(resp.Body)
			}
			resp.Body.Close()
			errMsg := string(errBody)
			if len(errMsg) > 200 {
				errMsg = errMsg[:200] + "..."
			}

			// Enhanced error logging with error type classification
			errorLabel := getErrorTypeLabel(resp.StatusCode, errMsg)

			// Special handling for rate limit errors (429)
			if isRateLimitError(resp.StatusCode) {
				retryAfter := getRetryAfterFromResponse(resp)
				logger.Warn("[%s] ⚠️ %s: %s", endpoint.Name, errorLabel, errMsg)
				if retryAfter > 0 {
					logger.Warn("[%s] Rate limit detected, waiting %v before retry...", endpoint.Name, retryAfter)
					time.Sleep(retryAfter)
				} else {
					// Exponential backoff if no Retry-After header
					backoffDuration := time.Duration(endpointAttempts*5) * time.Second
					if backoffDuration < 30*time.Second {
						backoffDuration = 30 * time.Second
					}
					if backoffDuration > 120*time.Second {
						backoffDuration = 120 * time.Second
					}
					logger.Warn("[%s] Rate limit detected, using backoff %v (attempt %d)", endpoint.Name, backoffDuration, endpointAttempts)
					time.Sleep(backoffDuration)
				}
				p.stats.RecordError(endpoint.Name)
				httpErr := fmt.Errorf("http %d: %s", resp.StatusCode, errMsg)
				p.recordEndpointFailure(endpoint.Name, httpErr)
				p.markRequestInactive(endpoint.Name)
				if endpointAttempts >= 2 {
					p.rotateEndpoint()
					endpointAttempts = 0
				}
				continue
			}

			// Special handling for service unavailable (503)
			if isServiceUnavailableError(resp.StatusCode) {
				logger.Warn("[%s] ⚠️ %s: %s", endpoint.Name, errorLabel, errMsg)
				logger.Warn("[%s] Service unavailable, will retry with exponential backoff", endpoint.Name)
				backoffDuration := time.Duration(endpointAttempts*3) * time.Second
				if backoffDuration < 10*time.Second {
					backoffDuration = 10 * time.Second
				}
				time.Sleep(backoffDuration)
				p.stats.RecordError(endpoint.Name)
				httpErr := fmt.Errorf("http %d: %s", resp.StatusCode, errMsg)
				p.recordEndpointFailure(endpoint.Name, httpErr)
				p.markRequestInactive(endpoint.Name)
				if endpointAttempts >= 2 {
					p.rotateEndpoint()
					endpointAttempts = 0
				}
				continue
			}

			// General error handling
			if resp.StatusCode == http.StatusNotFound {
				if strings.Contains(errMsg, "model") || strings.Contains(errMsg, "invalid_request_error") {
					logger.Warn("[%s] ⚠️ 模型不存在或无权限访问 (404)", endpoint.Name)
					logger.Warn("[%s] 模型 %s 可能不在 API Key 权限范围内，请检查端点配置", endpoint.Name, modelName)
					if valid, availableModels := p.ValidateModelForEndpoint(endpoint.Name, modelName); availableModels != nil {
						if !valid {
							if len(availableModels) <= 10 {
								logger.Warn("[%s] 可用模型列表: %v", endpoint.Name, availableModels)
							} else {
								logger.Warn("[%s] 可用模型列表(前10个): %v", endpoint.Name, availableModels[:10])
							}
						}
					}
				} else {
					logger.Warn("[%s] ⚠️ %s: %s", endpoint.Name, errorLabel, errMsg)
					p.suggestPossibleAPIPaths(endpoint, transformerName)
				}
			} else {
				logger.Warn("[%s] ⚠️ %s: %s", endpoint.Name, errorLabel, errMsg)
			}
			logger.DebugLog("[%s] Request failed %d: %s", endpoint.Name, resp.StatusCode, errMsg)
			p.markCredentialFailure(credentialID, resp.StatusCode, errMsg)
			p.recordCredentialUsage(credentialID, endpoint.Name, 0, 1, 0, 0)
			p.stats.RecordError(endpoint.Name)
			httpErr := fmt.Errorf("http %d: %s", resp.StatusCode, errMsg)
			p.recordEndpointFailure(endpoint.Name, httpErr)
			p.markRequestInactive(endpoint.Name)
			if endpointAttempts >= 2 {
				p.rotateEndpoint()
				endpointAttempts = 0
			}
			continue
		}

		var respBody []byte
		if resp.Header.Get("Content-Encoding") == "gzip" {
			respBody, _ = decompressGzip(resp.Body)
		} else {
			respBody, _ = io.ReadAll(resp.Body)
		}
		resp.Body.Close()
		skipCredentialPenalty := false

		// Token pool mode: on 401/403, invalidate current credential and retry within the same endpoint.
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && credentialID > 0 {
			errMsg := string(respBody)
			if len(errMsg) > 500 {
				errMsg = errMsg[:500] + "..."
			}
			if !shouldTreatCredentialAuthFailure(resp.StatusCode, errMsg) {
				skipCredentialPenalty = true
				logger.Warn("[%s] Upstream %d looks like route/gateway denial, skipping credential invalidation", endpoint.Name, resp.StatusCode)
			}
			if skipCredentialPenalty {
				p.stats.RecordError(endpoint.Name)
				p.markRequestInactive(endpoint.Name)
			} else {
				if selectedCredential != nil &&
					isCodexProviderType(selectedCredential.ProviderType) &&
					strings.TrimSpace(selectedCredential.RefreshToken) != "" &&
					!refreshedCredentialAttempts[credentialID] {
					refreshedCredentialAttempts[credentialID] = true
					refreshed, refreshErr := p.refreshCredential(endpoint, selectedCredential)
					if refreshErr == nil {
						logger.Info("[%s] Credential refreshed after %d, retrying with updated token (id=%d)", endpoint.Name, resp.StatusCode, credentialID)
						p.markRequestInactive(endpoint.Name)
						endpointAttempts = 0
						if refreshed != nil && refreshed.ID > 0 {
							refreshedCredentialAttempts[refreshed.ID] = true
						}
						continue
					}
					logger.Warn("[%s] Credential refresh failed after %d (id=%d): %v", endpoint.Name, resp.StatusCode, credentialID, refreshErr)
				}
				p.markCredentialFailure(credentialID, resp.StatusCode, errMsg)
				p.recordCredentialUsage(credentialID, endpoint.Name, 0, 1, 0, 0)
				p.stats.RecordError(endpoint.Name)
				p.markRequestInactive(endpoint.Name)
				endpointAttempts = 0
				logger.Warn("[%s] Credential auth failed (%d), retrying with next token", endpoint.Name, resp.StatusCode)
				continue
			}
		}

		p.markRequestInactive(endpoint.Name)
		// Log non-200 responses for debugging
		var errMsg string
		if resp.StatusCode != http.StatusOK {
			errMsg = string(respBody)
			if len(errMsg) > 500 {
				errMsg = errMsg[:500] + "..."
			}
			if resp.StatusCode == http.StatusBadRequest &&
				strings.Contains(errMsg, "api.responses.write") &&
				strings.Contains(transformerName, "openai2") {
				logger.Warn("[%s] Upstream rejected /v1/responses scope (api.responses.write). Try transformer=openai (chat/completions) for this token.", endpoint.Name)
			}
			if skipCredentialPenalty {
				p.markCredentialFailure(credentialID, 0, errMsg)
				p.recordCredentialUsage(credentialID, endpoint.Name, 0, 1, 0, 0)
			} else {
				p.markCredentialFailure(credentialID, resp.StatusCode, errMsg)
				p.recordCredentialUsage(credentialID, endpoint.Name, 0, 1, 0, 0)
			}
			logger.Warn("[%s] Response %d: %s", endpoint.Name, resp.StatusCode, errMsg)
			logger.DebugLog("[%s] Response %d: %s", endpoint.Name, resp.StatusCode, errMsg)
			if resp.StatusCode == http.StatusBadRequest && isUnsupportedModelError(errMsg) {
				if fallbackModel == "" {
					if hasTriedFallback {
						logger.Warn("[%s] Model fallback exhausted, both endpoint default and original models failed, trying next endpoint", endpoint.Name)
						p.markRequestInactive(endpoint.Name)
						endpointAttempts = 0
						continue
					} else if endpoint.Model != "" && endpoint.Model != originalModelName {
						logger.Info("[%s] Model '%s' not supported, falling back to endpoint default model: %s", endpoint.Name, modelName, endpoint.Model)
						fallbackModel = endpoint.Model
						fallbackOriginalModel = originalModelName
						hasTriedFallback = true
						p.markRequestInactive(endpoint.Name)
						endpointAttempts = 0
						continue
					} else if originalModelName != "" {
						logger.Info("[%s] Model '%s' not supported, trying original model: %s", endpoint.Name, modelName, originalModelName)
						fallbackModel = originalModelName
						hasTriedFallback = true
						p.markRequestInactive(endpoint.Name)
						endpointAttempts = 0
						continue
					}
				} else if fallbackOriginalModel != "" && fallbackModel != fallbackOriginalModel {
					logger.Info("[%s] Endpoint default model '%s' not supported, trying original model: %s", endpoint.Name, fallbackModel, fallbackOriginalModel)
					fallbackModel = fallbackOriginalModel
					p.markRequestInactive(endpoint.Name)
					endpointAttempts = 0
					continue
				}
			}
		}
		// Remove Content-Encoding header since we've decompressed
		for key, values := range resp.Header {
			if key == "Content-Encoding" || key == "Content-Length" {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		p.recordEndpointSuccess(endpoint.Name)
		return
	}

	p.writeJSONError(w, "All endpoints failed", http.StatusServiceUnavailable)
}

func (p *Proxy) writeJSONError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	errorResp := map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "upstream_error",
			"message": message,
		},
	}
	if jsonBytes, err := json.Marshal(errorResp); err == nil {
		w.Write(jsonBytes)
	}
}

func (p *Proxy) selectCredential(endpointName string) (*storage.EndpointCredential, error) {
	if p.storage == nil {
		return nil, nil
	}
	return p.storage.GetUsableEndpointCredential(endpointName, time.Now().UTC())
}

func (p *Proxy) markCredentialSuccess(credentialID int64) {
	if credentialID <= 0 || p.storage == nil {
		return
	}
	if err := p.storage.MarkCredentialSuccess(credentialID, time.Now().UTC()); err != nil {
		logger.Warn("Failed to mark credential success (id=%d): %v", credentialID, err)
	}
}

func (p *Proxy) recordCredentialUsage(credentialID int64, endpointName string, requests, errors, inputTokens, outputTokens int) {
	if credentialID <= 0 || p.storage == nil {
		return
	}
	if err := p.storage.UpsertCredentialUsage(credentialID, endpointName, requests, errors, inputTokens, outputTokens, time.Now().UTC()); err != nil {
		logger.Warn("Failed to record credential usage (id=%d): %v", credentialID, err)
	}
}

func (p *Proxy) markCredentialFailure(credentialID int64, statusCode int, errMsg string) {
	if credentialID <= 0 || p.storage == nil {
		return
	}
	if err := p.storage.MarkCredentialFailure(credentialID, statusCode, errMsg, time.Now().UTC()); err != nil {
		logger.Warn("Failed to mark credential failure (id=%d): %v", credentialID, err)
	}
}

func (p *Proxy) computeMaxRetries(endpoints []config.Endpoint) int {
	baseRetries := len(endpoints) * 2
	if p.storage == nil || len(endpoints) == 0 {
		return baseRetries
	}

	extraRetries := 0
	for _, endpoint := range endpoints {
		if !config.IsTokenPoolAuthMode(endpoint.AuthMode) {
			continue
		}

		stats, err := p.storage.GetTokenPoolStats(endpoint.Name)
		if err != nil {
			logger.Warn("[%s] Failed to load token pool stats: %v", endpoint.Name, err)
			continue
		}

		usable := stats.Active + stats.Expiring + stats.NeedRefresh
		if usable > 1 {
			extraRetries += usable - 1
		}
	}

	maxRetries := baseRetries + extraRetries
	if maxRetries < baseRetries {
		return baseRetries
	}
	return maxRetries
}

func shouldAggregateCodexStreaming(endpoint config.Endpoint, transformerName string) bool {
	if !strings.Contains(transformerName, "openai2") {
		return false
	}
	url := strings.ToLower(strings.TrimSpace(endpoint.APIUrl))
	return strings.Contains(url, "chatgpt.com/backend-api/codex")
}

// shouldHandleAsStreamingResponse determines if an upstream 200 response should be
// processed as SSE. Some Codex upstreams intermittently omit Content-Type even when
// stream=true and body is SSE.
func shouldHandleAsStreamingResponse(contentType string, clientRequestedStream bool, endpoint config.Endpoint, transformerName string) bool {
	if strings.Contains(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream") {
		return true
	}
	if !clientRequestedStream {
		return false
	}
	// Codex /responses may return SSE with an empty content-type header.
	if shouldAggregateCodexStreaming(endpoint, transformerName) {
		return true
	}
	return false
}

func shouldTreatCredentialAuthFailure(statusCode int, body string) bool {
	if statusCode == http.StatusUnauthorized {
		return true
	}
	if statusCode != http.StatusForbidden {
		return false
	}

	lower := strings.ToLower(strings.TrimSpace(body))
	if strings.HasPrefix(lower, "<!doctype html") ||
		strings.HasPrefix(lower, "<html") ||
		strings.Contains(lower, "<head>") ||
		strings.Contains(lower, "<body") {
		return false
	}
	return true
}

func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "eof") {
		return true
	}
	if strings.Contains(message, "timeout awaiting response headers") {
		return true
	}
	if strings.Contains(message, "i/o timeout") {
		return true
	}
	if strings.Contains(message, "connection reset by peer") {
		return true
	}
	return false
}
