package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fangfsz/ccg/internal/logger"
	"github.com/fangfsz/ccg/internal/tokencount"
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
