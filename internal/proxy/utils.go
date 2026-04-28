package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccg/internal/logger"
	"ccg/internal/tokencount"
)

// normalizeAPIUrl ensures the API URL has a protocol prefix
func normalizeAPIUrl(apiUrl string) string {
	if !strings.HasPrefix(apiUrl, "http://") && !strings.HasPrefix(apiUrl, "https://") {
		return "https://" + apiUrl
	}
	return apiUrl
}

// shouldRetry determines if a response should trigger a retry
func shouldRetry(statusCode int) bool {
	return statusCode != http.StatusOK &&
		statusCode != http.StatusBadRequest &&
		statusCode != http.StatusUnauthorized
}

// isRateLimitError checks if the status code indicates a rate limit error (429)
func isRateLimitError(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests
}

// isServiceUnavailableError checks if the status code indicates service unavailable (503)
func isServiceUnavailableError(statusCode int) bool {
	return statusCode == http.StatusServiceUnavailable
}

// parseRetryAfter parses the Retry-After header value and returns the duration to wait
// Supports both HTTP-date and delta-seconds formats per RFC 9110
func parseRetryAfter(headerValue string) time.Duration {
	if headerValue == "" {
		return 0
	}

	// Try parsing as delta-seconds first (most common for APIs)
	if seconds, err := strconv.Atoi(headerValue); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	// Try parsing as HTTP-date (e.g., "Wed, 31 Dec 2025 23:59:59 GMT")
	if t, err := time.Parse(time.RFC1123, headerValue); err == nil {
		waitDuration := t.Sub(time.Now())
		if waitDuration > 0 {
			return waitDuration
		}
	}

	// Try other common HTTP date formats
	dateFormats := []string{
		time.RFC1123Z,
		"Mon, 02 Jan 2006 15:04:05 MST",
		"Mon, 02 Jan 2006 15:04:05 GMT",
	}
	for _, format := range dateFormats {
		if t, err := time.Parse(format, headerValue); err == nil {
			waitDuration := t.Sub(time.Now())
			if waitDuration > 0 {
				return waitDuration
			}
		}
	}

	return 0
}

// getRetryAfterFromResponse extracts Retry-After duration from HTTP response headers
func getRetryAfterFromResponse(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}

	// Check Retry-After header
	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter != "" {
		duration := parseRetryAfter(retryAfter)
		if duration > 0 {
			logger.Debug("[RateLimit] Retry-After header: %v", duration)
			return duration
		}
	}

	// For 429 responses, extract retry info from error message if available
	if resp.StatusCode == http.StatusTooManyRequests {
		return 60 * time.Second // Default fallback for 429 without Retry-After
	}

	return 0
}

// classifyErrorType classifies an HTTP error into a human-readable category
func classifyErrorType(statusCode int, errMsg string) string {
	switch statusCode {
	case http.StatusTooManyRequests:
		if strings.Contains(errMsg, "rate limit") ||
			strings.Contains(errMsg, "请求数限制") ||
			strings.Contains(errMsg, "请求限制") {
			return "rate_limit"
		}
		return "rate_limit"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	case http.StatusBadGateway:
		return "bad_gateway"
	case http.StatusGatewayTimeout:
		return "gateway_timeout"
	case http.StatusNotFound:
		if strings.Contains(errMsg, "model") {
			return "model_not_found"
		}
		return "not_found"
	case http.StatusInternalServerError:
		return "internal_error"
	default:
		return "unknown"
	}
}

// getErrorTypeLabel returns a human-readable label for the error type
func getErrorTypeLabel(statusCode int, errMsg string) string {
	errType := classifyErrorType(statusCode, errMsg)
	switch errType {
	case "rate_limit":
		return "速率限制 (429)"
	case "service_unavailable":
		return "服务不可用 (503)"
	case "bad_gateway":
		return "网关错误 (502)"
	case "gateway_timeout":
		return "网关超时 (504)"
	case "model_not_found":
		return "模型不存在 (404)"
	case "not_found":
		return "资源不存在 (404)"
	case "internal_error":
		return "服务器内部错误 (500)"
	default:
		return "未知错误"
	}
}

// isUnsupportedModelError checks if the error message indicates an unsupported model error
func isUnsupportedModelError(errMsg string) bool {
	errLower := strings.ToLower(errMsg)
	modelErrorIndicators := []string{
		"model not found",
		"model not available",
		"model not support",
		"unsupported model",
		"invalid model",
		"unknown model",
		"model does not exist",
		"model unavailable",
		"model is not available",
		"is not a valid model",
		"is not supported",
		"model_id",
	}
	for _, indicator := range modelErrorIndicators {
		if strings.Contains(errLower, indicator) {
			return true
		}
	}
	return false
}

// cleanIncompleteToolCalls removes incomplete tool_use blocks from request
func cleanIncompleteToolCalls(bodyBytes []byte) ([]byte, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return bodyBytes, err
	}

	messages, ok := req["messages"].([]interface{})
	if !ok {
		return bodyBytes, nil
	}

	hasIncomplete := false
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		if role != "assistant" {
			break
		}

		content, ok := msg["content"].([]interface{})
		if !ok {
			break
		}

		var cleanedContent []interface{}
		for _, block := range content {
			blockMap, ok := block.(map[string]interface{})
			if !ok {
				cleanedContent = append(cleanedContent, block)
				continue
			}

			blockType, _ := blockMap["type"].(string)
			if blockType == "tool_use" {
				if input, hasInput := blockMap["input"]; !hasInput || input == nil {
					logger.Debug("Removing incomplete tool_use block without input")
					hasIncomplete = true
					continue
				}
			}
			cleanedContent = append(cleanedContent, block)
		}

		if hasIncomplete {
			if len(cleanedContent) == 0 {
				messages = append(messages[:i], messages[i+1:]...)
			} else {
				msg["content"] = cleanedContent
			}
		}
		break
	}

	if !hasIncomplete {
		return bodyBytes, nil
	}

	req["messages"] = messages
	return json.Marshal(req)
}

// estimateInputTokens estimates input tokens from request body
func (p *Proxy) estimateInputTokens(bodyBytes []byte) int {
	var req tokencount.CountTokensRequest
	if json.Unmarshal(bodyBytes, &req) == nil {
		return tokencount.EstimateInputTokens(&req)
	}
	return 0
}

// estimateTokens estimates tokens when API doesn't provide usage
func (p *Proxy) estimateTokens(bodyBytes []byte, outputText string, inputTokens, outputTokens int, endpointName string) (int, int) {
	if inputTokens == 0 {
		var req tokencount.CountTokensRequest
		if json.Unmarshal(bodyBytes, &req) == nil {
			inputTokens = tokencount.EstimateInputTokens(&req)
			logger.Debug("[%s] Estimated input tokens: %d", endpointName, inputTokens)
		}
	}

	if outputTokens == 0 && outputText != "" {
		outputTokens = tokencount.EstimateOutputTokens(outputText)
		logger.Debug("[%s] Estimated output tokens: %d", endpointName, outputTokens)
	}

	return inputTokens, outputTokens
}
