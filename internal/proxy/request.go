package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/proxy"

	"ccg/internal/config"
	"ccg/internal/logger"
	"ccg/internal/storage"
	"ccg/internal/transformer"
	"ccg/internal/transformer/cc"
	"ccg/internal/transformer/cx/chat"
	"ccg/internal/transformer/cx/responses"
)

const (
	codexClientVersion = "0.101.0"
	codexUserAgent     = "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
)

// prepareTransformerForClient creates transformer based on client format and endpoint
func prepareTransformerForClient(clientFormat ClientFormat, endpoint config.Endpoint) (transformer.Transformer, error) {
	endpointTransformer := strings.ToLower(strings.TrimSpace(endpoint.Transformer))

	if endpointTransformer == "auto" || endpointTransformer == "" {
		inferredTransformer := inferTransformer(endpoint, clientFormat)
		logger.Debug("[%s] Transformer auto-detection: endpointTransformer=%s → inferred=%s",
			endpoint.Name, endpoint.Transformer, inferredTransformer)
		endpointTransformer = inferredTransformer
	}

	switch clientFormat {
	case ClientFormatClaude:
		return prepareCCTransformer(endpoint, endpointTransformer)
	case ClientFormatOpenAIChat:
		return prepareCxChatTransformer(endpoint, endpointTransformer)
	case ClientFormatOpenAIResponses:
		return prepareCxRespTransformer(endpoint, endpointTransformer)
	}

	return nil, fmt.Errorf("unsupported client format: %s", clientFormat)
}

// inferTransformer automatically selects the best transformer based on endpoint configuration
// It considers ProviderType, model name patterns, and client format to make an intelligent decision
//
// Key decision logic:
// 1. cliproxyapi: always passthrough (no transformation)
// 2. Chinese models supporting Claude natively (DeepSeek, MiniMax, Qwen):
//   - If clientFormat is Claude: passthrough (no transformation needed)
//   - If clientFormat is OpenAI: claude (convert Claude → OpenAI)
//
// 3. Chinese models NOT supporting Claude natively (GLM, etc.):
//   - Always use openai (convert whatever → OpenAI)
//
// 4. OpenAI/Claude/Gemini models: use appropriate native format
// 5. Relay providers (oneapi, newapi, sub2api): use openai (they accept OpenAI format)
func inferTransformer(endpoint config.Endpoint, clientFormat ClientFormat) string {
	providerType := strings.ToLower(strings.TrimSpace(endpoint.ProviderType))
	modelName := strings.ToLower(strings.TrimSpace(endpoint.Model))

	logger.Debug("[Transformer] Inferring: provider=%s, model=%s, clientFormat=%s",
		providerType, modelName, clientFormat)

	// 1. cliproxyapi always passthrough
	if providerType == "cliproxyapi" {
		logger.Debug("[Transformer] cliproxyapi → passthrough")
		return "passthrough"
	}

	// 2. Chinese models that support Claude natively
	if isClaudeCompatibleChineseModel(modelName) {
		logger.Debug("[Transformer] Claude-compatible Chinese model detected: %s", modelName)
		if clientFormat == ClientFormatClaude {
			logger.Debug("[Transformer] Client wants Claude, model supports it natively → passthrough")
			return "passthrough"
		}
		// Client wants OpenAI but model supports Claude natively
		logger.Debug("[Transformer] Client wants %s, converting from Claude → %s", clientFormat, "openai/claude")
		return "claude"
	}

	// 3. Chinese models that do NOT support Claude natively (GLM, etc.)
	if isOpenAICompatibleChineseModel(modelName) {
		logger.Debug("[Transformer] OpenAI-only Chinese model: %s → openai", modelName)
		return "openai"
	}

	// 4. Relay providers - use OpenAI format
	if isRelayProvider(providerType) {
		logger.Debug("[Transformer] Relay provider (%s) → openai", providerType)
		return "openai"
	}

	// 5. Native providers with native model types
	if providerType == "native" || providerType == "" {
		if isOpenAIModel(modelName) {
			logger.Debug("[Transformer] OpenAI model → openai")
			return "openai"
		}
		if isClaudeModel(modelName) {
			if clientFormat == ClientFormatClaude {
				logger.Debug("[Transformer] Claude model + Claude client → passthrough")
				return "passthrough"
			}
			logger.Debug("[Transformer] Claude model + non-Claude client → claude")
			return "claude"
		}
		if isGeminiModel(modelName) {
			logger.Debug("[Transformer] Gemini model → gemini")
			return "gemini"
		}
	}

	logger.Debug("[Transformer] Default → openai")
	return "openai"
}

// isRelayProvider checks if the provider type is a known relay/proxy service
func isRelayProvider(providerType string) bool {
	relayProviders := []string{"oneapi", "newapi", "sub2api"}
	for _, p := range relayProviders {
		if providerType == p {
			return true
		}
	}
	return false
}

// shouldSkipProbe checks if we should skip API path probing for this endpoint
// Probing can trigger rate limits on third-party relays and may not work with GET requests
// for endpoints that only support POST. For known relay providers, probing is generally safe.
func shouldSkipProbe(providerType string) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))

	// Known relay providers support probing well
	if isRelayProvider(providerType) {
		return false
	}

	// cliproxyapi should skip probing (typically requires POST)
	if providerType == "cliproxyapi" {
		return true
	}

	// Empty or "native" providers - these are usually official APIs that support probing
	if providerType == "" || providerType == "native" {
		return false
	}

	// For any other third-party relay, skip probing to avoid rate limits
	// The system will rely on inference from ProviderType and transformer
	return true
}

// inferAPIPathFromConfig infers the API path based on ProviderType and transformer
// This is used when probing is skipped to avoid rate limits
func inferAPIPathFromConfig(providerType string, transformer string) string {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	transformer = strings.ToLower(strings.TrimSpace(transformer))

	// Known relay providers
	if isRelayProvider(providerType) {
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
	}

	// cliproxyapi - supports POST only
	if providerType == "cliproxyapi" {
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

	// Native or empty provider type - use transformer to determine
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

// isChineseModel checks if the model name indicates a Chinese domestic model
// Note: Not all Chinese models support Claude /v1/messages natively.
// Use isClaudeCompatibleChineseModel to check Claude API compatibility.
func isChineseModel(modelName string) bool {
	chinesePrefixes := []string{
		"glm-", "glm.", // Zhipu AI (智谱AI) - 不支持Claude原生
		"qwen-", "qwen.", // Alibaba (阿里通义) - 支持Claude原生
		"deepseek-", // DeepSeek (深度求索) - 支持Claude原生
		"minimax-",  // MiniMax (海螺AI) - 支持Claude原生
		"moonshot-", // Moonshot (月之暗面)
		"spark-",    // iFlytek (科大讯飞星火)
		"ernie-",    // Baidu (百度文心)
		"hunyuan-",  // Tencent (腾讯混元)
		"lingxi-",   // Lingxi
		"step-",     // StepFun (阶跃星辰)
		"abab-",     // MiniMax
		"local-",    // Local models
	}
	modelName = strings.ToLower(modelName)
	for _, prefix := range chinesePrefixes {
		if strings.HasPrefix(modelName, prefix) {
			return true
		}
	}
	return false
}

// isClaudeCompatibleChineseModel checks if a Chinese domestic model supports Claude /v1/messages natively
// Based on research as of 2026-04-28:
// - DeepSeek V4/V3: Supported via https://api.deepseek.com/anthropic/v1/messages
// - MiniMax M2.7/M2.1: Supported via https://api.minimax.io/anthropic/v1/messages
// - Qwen series: Supported via https://dashscope.aliyuncs.com/apps/anthropic
// - GLM series: NOT supported natively, requires OpenAI format or third-party relay
func isClaudeCompatibleChineseModel(modelName string) bool {
	modelName = strings.ToLower(modelName)

	// Models that support Claude /v1/messages natively
	// Use prefix matching with common separators: -, ., /
	if strings.HasPrefix(modelName, "deepseek-") ||
		strings.HasPrefix(modelName, "deepseek.") ||
		strings.HasPrefix(modelName, "minimax-") ||
		strings.HasPrefix(modelName, "minimax.") ||
		strings.HasPrefix(modelName, "qwen-") ||
		strings.HasPrefix(modelName, "qwen.") ||
		strings.HasPrefix(modelName, "qwq-") ||
		strings.HasPrefix(modelName, "qwq.") {
		return true
	}

	// Handle Qwen models with version numbers like qwen3.6-plus, qwen3.5-plus
	// These have pattern: qwen<digit>.<digit>...
	if strings.HasPrefix(modelName, "qwen") {
		// Check if after "qwen" there's a digit (version number)
		if len(modelName) > 4 {
			c := modelName[4]
			if c >= '0' && c <= '9' {
				return true
			}
		}
	}

	return false
}

// isOpenAICompatibleChineseModel checks if a Chinese model requires OpenAI format
// These models do NOT support Claude /v1/messages natively and need format conversion
func isOpenAICompatibleChineseModel(modelName string) bool {
	return isChineseModel(modelName) && !isClaudeCompatibleChineseModel(modelName)
}

// isOpenAIModel checks if the model name indicates an OpenAI model
func isOpenAIModel(modelName string) bool {
	openaiPrefixes := []string{"gpt-3", "gpt-4", "gpt-5", "gpt5", "o1", "o2", "o3"}
	modelName = strings.ToLower(modelName)
	for _, prefix := range openaiPrefixes {
		if strings.HasPrefix(modelName, prefix) {
			return true
		}
	}
	return false
}

// isClaudeModel checks if the model name indicates an Anthropic Claude model
func isClaudeModel(modelName string) bool {
	claudePrefixes := []string{"claude-", "claude."}
	modelName = strings.ToLower(modelName)
	for _, prefix := range claudePrefixes {
		if strings.HasPrefix(modelName, prefix) {
			return true
		}
	}
	return false
}

// isGeminiModel checks if the model name indicates a Google Gemini model
func isGeminiModel(modelName string) bool {
	geminiPrefixes := []string{"gemini-", "gemini."}
	modelName = strings.ToLower(modelName)
	for _, prefix := range geminiPrefixes {
		if strings.HasPrefix(modelName, prefix) {
			return true
		}
	}
	return false
}

// prepareCCTransformer creates transformer for Claude Code client
func prepareCCTransformer(endpoint config.Endpoint, endpointTransformer string) (transformer.Transformer, error) {
	switch endpointTransformer {
	case "passthrough", "claude":
		if endpoint.Model != "" {
			logger.Debug("[%s] Using cc_passthrough with model override: %s", endpoint.Name, endpoint.Model)
			return cc.NewClaudeTransformerWithModel(endpoint.Model), nil
		}
		return cc.NewClaudeTransformer(), nil
	case "openai":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("OpenAI transformer requires model field")
		}
		return cc.NewOpenAITransformer(endpoint.Model), nil
	case "openai2":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("OpenAI2 transformer requires model field")
		}
		return cc.NewOpenAI2Transformer(endpoint.Model), nil
	case "gemini":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("Gemini transformer requires model field")
		}
		return cc.NewGeminiTransformer(endpoint.Model), nil
	default:
		return nil, fmt.Errorf("unsupported endpoint transformer: %s", endpointTransformer)
	}
}

// prepareCxChatTransformer creates transformer for Codex Chat API client
func prepareCxChatTransformer(endpoint config.Endpoint, endpointTransformer string) (transformer.Transformer, error) {
	switch endpointTransformer {
	case "passthrough", "claude":
		model := endpoint.Model
		if model == "" {
			model = "claude-sonnet-4-6"
		}
		return chat.NewClaudeTransformer(model), nil
	case "openai":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("OpenAI transformer requires model field")
		}
		return chat.NewOpenAITransformer(endpoint.Model), nil
	case "openai2":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("OpenAI2 transformer requires model field")
		}
		return chat.NewOpenAI2Transformer(endpoint.Model), nil
	case "gemini":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("Gemini transformer requires model field")
		}
		return chat.NewGeminiTransformer(endpoint.Model), nil
	default:
		return nil, fmt.Errorf("unsupported endpoint transformer for Codex Chat: %s", endpointTransformer)
	}
}

// prepareCxRespTransformer creates transformer for Codex Responses API client
func prepareCxRespTransformer(endpoint config.Endpoint, endpointTransformer string) (transformer.Transformer, error) {
	switch endpointTransformer {
	case "passthrough", "claude":
		model := endpoint.Model
		if model == "" {
			model = "claude-sonnet-4-6"
		}
		return responses.NewClaudeTransformer(model), nil
	case "openai":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("OpenAI transformer requires model field")
		}
		return responses.NewOpenAITransformer(endpoint.Model), nil
	case "openai2":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("OpenAI2 transformer requires model field")
		}
		return responses.NewOpenAI2Transformer(endpoint.Model), nil
	case "gemini":
		if endpoint.Model == "" {
			return nil, fmt.Errorf("Gemini transformer requires model field")
		}
		return responses.NewGeminiTransformer(endpoint.Model), nil
	default:
		return nil, fmt.Errorf("unsupported endpoint transformer for Codex Responses: %s", endpointTransformer)
	}
}

// getTargetPath determines the target API path based on transformer name and ProviderType
func getTargetPath(originalPath string, endpoint config.Endpoint, transformedBody []byte, transformerName string) string {
	preferredPath := getPreferredAPIPath(endpoint.ProviderType, transformerName, endpoint.CustomPath)
	if preferredPath != "" {
		return preferredPath
	}

	switch transformerName {
	case "cc_claude", "cx_chat_claude", "cx_resp_claude":
		return "/v1/messages"
	case "cc_openai", "cx_chat_openai", "cx_resp_openai":
		return "/v1/chat/completions"
	case "cc_openai2", "cx_resp_openai2", "cx_chat_openai2":
		return "/v1/responses"
	case "cc_gemini", "cx_chat_gemini", "cx_resp_gemini":
		var geminiReq struct {
			Stream bool `json:"stream"`
		}
		json.Unmarshal(transformedBody, &geminiReq)
		if geminiReq.Stream {
			return fmt.Sprintf("/v1beta/models/%s:streamGenerateContent", endpoint.Model)
		}
		return fmt.Sprintf("/v1beta/models/%s:generateContent", endpoint.Model)
	}
	return originalPath
}

// buildProxyRequest creates an HTTP request for the target API
func buildProxyRequest(r *http.Request, endpoint config.Endpoint, apiKey string, transformedBody []byte, transformerName string, credential *storage.EndpointCredential) (*http.Request, error) {
	targetPath := getTargetPath(r.URL.Path, endpoint, transformedBody, transformerName)
	if targetPath == "" {
		targetPath = r.URL.Path
	}

	normalizedAPIUrl := normalizeAPIUrl(endpoint.APIUrl)
	targetPath = normalizeTargetPathForBaseURL(normalizedAPIUrl, targetPath)
	requestBody := transformedBody
	if isCodexBackendBaseURL(normalizedAPIUrl) && isResponsesPath(targetPath) {
		requestBody = ensureCodexResponsesPayload(requestBody)
	}
	targetURL := fmt.Sprintf("%s%s", normalizedAPIUrl, targetPath)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}

	// Copy headers (except Host and Accept-Encoding)
	for key, values := range r.Header {
		if key == "Host" || key == "Accept-Encoding" {
			continue
		}
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// Force gzip or no compression to avoid unsupported encodings (e.g., brotli)
	proxyReq.Header.Set("Accept-Encoding", "gzip, identity")

	// Set authentication based on transformer type
	switch transformerName {
	case "cc_openai", "cc_openai2", "cx_chat_openai", "cx_chat_openai2", "cx_resp_openai", "cx_resp_openai2":
		proxyReq.Header.Set("Authorization", "Bearer "+apiKey)
	case "cc_gemini", "cx_chat_gemini", "cx_resp_gemini":
		q := proxyReq.URL.Query()
		q.Set("key", apiKey)
		q.Set("alt", "sse")
		proxyReq.URL.RawQuery = q.Encode()
	default:
		// Claude endpoints
		proxyReq.Header.Set("x-api-key", apiKey)
		proxyReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	// Set Host header
	if parsedBase, err := url.Parse(normalizedAPIUrl); err == nil && strings.TrimSpace(parsedBase.Host) != "" {
		proxyReq.Header.Set("Host", parsedBase.Host)
	}
	applyCodexCredentialHeaders(proxyReq, credential, requestBody)

	return proxyReq, nil
}

func applyCodexCredentialHeaders(req *http.Request, credential *storage.EndpointCredential, payload []byte) {
	if req == nil || credential == nil {
		return
	}
	if !isCodexProviderType(credential.ProviderType) {
		return
	}
	if !isResponsesPath(req.URL.Path) {
		return
	}

	// Match Codex client headers for oauth credentials.
	ensureHeader(req.Header, "Version", codexClientVersion)
	ensureHeader(req.Header, "Session_id", uuid.NewString())
	ensureHeader(req.Header, "User-Agent", codexUserAgent)

	if isStreamingRequest(payload) {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("Originator", "codex_cli_rs")
	if accountID := strings.TrimSpace(credential.AccountID); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
}

func ensureHeader(headers http.Header, key, value string) {
	if headers == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return
	}
	if strings.TrimSpace(headers.Get(key)) == "" {
		headers.Set(key, value)
	}
}

func isResponsesPath(path string) bool {
	trimmed := strings.TrimSpace(path)
	return strings.HasSuffix(trimmed, "/responses") || strings.HasSuffix(trimmed, "/responses/compact")
}

func isStreamingRequest(payload []byte) bool {
	var streamReq struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(payload, &streamReq); err != nil {
		return false
	}
	return streamReq.Stream
}

func isCodexProviderType(providerType string) bool {
	p := strings.ToLower(strings.TrimSpace(providerType))
	return p == "" || p == "codex"
}

// normalizeTargetPathForBaseURL adjusts OpenAI Responses paths for Codex backend base URLs.
// This is endpoint URL compatibility handling and is independent from auth mode.
func normalizeTargetPathForBaseURL(baseURL, targetPath string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed == nil {
		return targetPath
	}

	cleanPath := path.Clean(strings.TrimSpace(parsed.Path))
	isCodexBackend := strings.HasSuffix(cleanPath, "/backend-api/codex")
	if !isCodexBackend {
		return targetPath
	}

	switch strings.TrimSpace(targetPath) {
	case "/v1/responses":
		return "/responses"
	case "/v1/responses/compact":
		return "/responses/compact"
	default:
		return targetPath
	}
}

func isCodexBackendBaseURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed == nil {
		return false
	}
	cleanPath := path.Clean(strings.TrimSpace(parsed.Path))
	return strings.HasSuffix(cleanPath, "/backend-api/codex")
}

func ensureCodexResponsesPayload(payload []byte) []byte {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || strings.HasPrefix(trimmed, "[") {
		return payload
	}

	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	body["store"] = false
	body["stream"] = true
	if _, ok := body["instructions"]; !ok {
		body["instructions"] = ""
	}
	updated, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return updated
}

func overrideModelInPayload(payload []byte, model string) []byte {
	if strings.TrimSpace(model) == "" {
		return payload
	}
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || strings.HasPrefix(trimmed, "[") {
		return payload
	}

	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	body["model"] = model
	updated, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return updated
}

// sendRequest sends the HTTP request and returns the response
func sendRequest(ctx context.Context, proxyReq *http.Request, httpClient *http.Client, cfg *config.Config) (*http.Response, error) {
	proxyReq = proxyReq.WithContext(ctx)

	proxyURL := resolveProxyURLForRequest(cfg, proxyReq.URL)
	// Apply proxy if configured
	if strings.TrimSpace(proxyURL) != "" {
		// Clone the client and replace transport for this request
		clientWithProxy := &http.Client{
			Timeout: httpClient.Timeout,
		}

		transport, err := CreateProxyTransport(proxyURL)
		if err != nil {
			logger.Warn("Failed to create proxy transport: %v, using direct connection", err)
			clientWithProxy.Transport = httpClient.Transport
		} else {
			clientWithProxy.Transport = transport
		}

		return clientWithProxy.Do(proxyReq)
	}

	return httpClient.Do(proxyReq)
}

func resolveProxyURLForRequest(cfg *config.Config, targetURL *url.URL) string {
	if cfg == nil {
		return ""
	}
	if isCodexRequestURL(targetURL) {
		if codexProxy := cfg.GetCodexProxy(); codexProxy != nil && strings.TrimSpace(codexProxy.URL) != "" {
			return codexProxy.URL
		}
	}
	if proxyCfg := cfg.GetProxy(); proxyCfg != nil && strings.TrimSpace(proxyCfg.URL) != "" {
		return proxyCfg.URL
	}
	return ""
}

func isCodexRequestURL(targetURL *url.URL) bool {
	if targetURL == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(targetURL.Host))
	if host != "chatgpt.com" {
		return false
	}
	cleanPath := path.Clean(strings.TrimSpace(targetURL.Path))
	return strings.Contains(cleanPath, "/backend-api/codex")
}

// CreateProxyTransport creates an http.Transport with proxy support
func CreateProxyTransport(proxyURL string) (*http.Transport, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}

	transport := &http.Transport{
		MaxIdleConns:           100,
		MaxIdleConnsPerHost:    10,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  1 * time.Second,
		ResponseHeaderTimeout:  90 * time.Second,
		WriteBufferSize:        128 * 1024, // 128KB write buffer for large SSE streams
		ReadBufferSize:         128 * 1024, // 128KB read buffer for large SSE streams
		MaxResponseHeaderBytes: 64 * 1024,  // 64KB max response headers
	}

	switch parsed.Scheme {
	case "socks5", "socks5h":
		auth := &proxy.Auth{}
		if parsed.User != nil {
			auth.User = parsed.User.Username()
			auth.Password, _ = parsed.User.Password()
		} else {
			auth = nil
		}
		dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
		}
		transport.Dial = dialer.Dial
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", parsed.Scheme)
	}

	return transport, nil
}
