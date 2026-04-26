package convert

import (
	"encoding/json"
	"testing"
)

func TestClaudeReqToGemini(t *testing.T) {
	claudeReq := `{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": "Hello, how are you?"}
		],
		"max_tokens": 1024
	}`

	geminiReqBytes, err := ClaudeReqToGemini([]byte(claudeReq), "gemini-2.0-flash")
	if err != nil {
		t.Fatalf("ClaudeReqToGemini failed: %v", err)
	}

	var geminiReq map[string]interface{}
	if err := json.Unmarshal(geminiReqBytes, &geminiReq); err != nil {
		t.Fatalf("Failed to unmarshal Gemini request: %v", err)
	}

	contents, ok := geminiReq["contents"].([]interface{})
	if !ok {
		t.Fatal("Expected contents to be an array")
	}
	if len(contents) != 1 {
		t.Fatalf("Expected 1 content block, got %d", len(contents))
	}

	genConfig, ok := geminiReq["generationConfig"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected generationConfig")
	}
	if genConfig["maxOutputTokens"] != float64(1024) {
		t.Errorf("Expected maxOutputTokens to be 1024, got %v", genConfig["maxOutputTokens"])
	}
}

func TestClaudeReqToGeminiWithSystem(t *testing.T) {
	claudeReq := `{
		"model": "claude-sonnet-4-20250514",
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	geminiReqBytes, err := ClaudeReqToGemini([]byte(claudeReq), "gemini-2.0-flash")
	if err != nil {
		t.Fatalf("ClaudeReqToGemini failed: %v", err)
	}

	var geminiReq map[string]interface{}
	if err := json.Unmarshal(geminiReqBytes, &geminiReq); err != nil {
		t.Fatalf("Failed to unmarshal Gemini request: %v", err)
	}

	systemInstruction, ok := geminiReq["systemInstruction"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected systemInstruction")
	}
	parts, ok := systemInstruction["parts"].([]interface{})
	if !ok {
		t.Fatal("Expected parts in systemInstruction")
	}
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part in systemInstruction, got %d", len(parts))
	}
}

func TestGeminiRespToClaude(t *testing.T) {
	geminiResp := `{
		"candidates": [{
			"content": {
				"role": "model",
				"parts": [{"text": "Hello! I'm doing well, thank you."}]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 10,
			"candidatesTokenCount": 12,
			"totalTokenCount": 22
		}
	}`

	claudeRespBytes, err := GeminiRespToClaude([]byte(geminiResp))
	if err != nil {
		t.Fatalf("GeminiRespToClaude failed: %v", err)
	}

	var claudeResp map[string]interface{}
	if err := json.Unmarshal(claudeRespBytes, &claudeResp); err != nil {
		t.Fatalf("Failed to unmarshal Claude response: %v", err)
	}

	content, ok := claudeResp["content"].([]interface{})
	if !ok {
		t.Fatal("Expected content to be an array")
	}
	if len(content) != 1 {
		t.Fatalf("Expected 1 content block, got %d", len(content))
	}

	block := content[0].(map[string]interface{})
	if block["type"] != "text" {
		t.Errorf("Expected text block, got %v", block["type"])
	}

	usage, ok := claudeResp["usage"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected usage")
	}
	if usage["input_tokens"].(float64) != 10 {
		t.Errorf("Expected input_tokens to be 10, got %v", usage["input_tokens"])
	}
	if usage["output_tokens"].(float64) != 12 {
		t.Errorf("Expected output_tokens to be 12, got %v", usage["output_tokens"])
	}
}

func TestGeminiRespToClaudeWithFunctionCall(t *testing.T) {
	geminiResp := `{
		"candidates": [{
			"content": {
				"role": "model",
				"parts": [
					{"functionCall": {"name": "get_weather", "args": {"city": "Beijing"}}},
					{"text": "The weather in Beijing is sunny."}
				]
			},
			"finishReason": "TOOL_CODE"
		}],
		"usageMetadata": {
			"promptTokenCount": 20,
			"candidatesTokenCount": 15,
			"totalTokenCount": 35
		}
	}`

	claudeRespBytes, err := GeminiRespToClaude([]byte(geminiResp))
	if err != nil {
		t.Fatalf("GeminiRespToClaude failed: %v", err)
	}

	var claudeResp map[string]interface{}
	if err := json.Unmarshal(claudeRespBytes, &claudeResp); err != nil {
		t.Fatalf("Failed to unmarshal Claude response: %v", err)
	}

	content, ok := claudeResp["content"].([]interface{})
	if !ok {
		t.Fatal("Expected content to be an array")
	}
	if len(content) != 2 {
		t.Fatalf("Expected 2 content blocks, got %d", len(content))
	}

	block1 := content[0].(map[string]interface{})
	if block1["type"] != "tool_use" {
		t.Errorf("Expected first block to be tool_use, got %v", block1["type"])
	}
	if block1["name"] != "get_weather" {
		t.Errorf("Expected function name to be get_weather, got %v", block1["name"])
	}

	stopReason, ok := claudeResp["stop_reason"].(string)
	if !ok {
		t.Fatal("Expected stop_reason")
	}
	if stopReason != "tool_use" {
		t.Errorf("Expected stop_reason to be tool_use, got %s", stopReason)
	}
}

func TestClaudeRespToGemini(t *testing.T) {
	claudeResp := `{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [
			{"type": "text", "text": "Hello!"}
		],
		"model": "claude-sonnet-4-20250514",
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": 5,
			"output_tokens": 10
		}
	}`

	geminiRespBytes, err := ClaudeRespToGemini([]byte(claudeResp))
	if err != nil {
		t.Fatalf("ClaudeRespToGemini failed: %v", err)
	}

	var geminiResp map[string]interface{}
	if err := json.Unmarshal(geminiRespBytes, &geminiResp); err != nil {
		t.Fatalf("Failed to unmarshal Gemini response: %v", err)
	}

	candidates, ok := geminiResp["candidates"].([]interface{})
	if !ok {
		t.Fatal("Expected candidates")
	}
	if len(candidates) != 1 {
		t.Fatalf("Expected 1 candidate, got %d", len(candidates))
	}

	usage, ok := geminiResp["usageMetadata"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected usageMetadata")
	}
	if usage["promptTokenCount"].(float64) != 5 {
		t.Errorf("Expected promptTokenCount to be 5, got %v", usage["promptTokenCount"])
	}
}

func TestClaudeReqToGeminiWithTools(t *testing.T) {
	claudeReq := `{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": "What's the weather in Beijing?"}
		],
		"tools": [
			{
				"name": "get_weather",
				"description": "Get weather for a city",
				"input_schema": {
					"type": "object",
					"properties": {
						"city": {"type": "string"}
					}
				}
			}
		]
	}`

	geminiReqBytes, err := ClaudeReqToGemini([]byte(claudeReq), "gemini-2.0-flash")
	if err != nil {
		t.Fatalf("ClaudeReqToGemini failed: %v", err)
	}

	var geminiReq map[string]interface{}
	if err := json.Unmarshal(geminiReqBytes, &geminiReq); err != nil {
		t.Fatalf("Failed to unmarshal Gemini request: %v", err)
	}

	tools, ok := geminiReq["tools"].([]interface{})
	if !ok {
		t.Fatal("Expected tools")
	}
	if len(tools) != 1 {
		t.Fatalf("Expected 1 tool, got %d", len(tools))
	}

	toolConfig, ok := geminiReq["toolConfig"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected toolConfig")
	}
	fcConfig, ok := toolConfig["functionCallingConfig"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected functionCallingConfig in toolConfig")
	}
	if fcConfig["mode"] != "AUTO" {
		t.Errorf("Expected mode to be AUTO, got %v", fcConfig["mode"])
	}
}
