package proxy

import (
	"testing"
)

func TestIsClaudeCompatibleChineseModel(t *testing.T) {
	tests := []struct {
		modelName string
		expected  bool
		desc      string
	}{
		// DeepSeek - 支持 Claude 原生
		{"deepseek-v4", true, "DeepSeek V4 支持 Claude"},
		{"deepseek-v3", true, "DeepSeek V3 支持 Claude"},
		{"deepseek-chat", true, "DeepSeek Chat 支持 Claude"},
		{"DEEPSEEK-V4", true, "DeepSeek 大写应不敏感"},

		// MiniMax - 支持 Claude 原生
		{"minimax-m2.7", true, "MiniMax M2.7 支持 Claude"},
		{"minimax-m2.1", true, "MiniMax M2.1 支持 Claude"},
		{"minimax-m2", true, "MiniMax M2 支持 Claude"},
		{"minimax-01", true, "MiniMax 01 支持 Claude"},

		// Qwen - 支持 Claude 原生
		{"qwen-plus", true, "Qwen Plus 支持 Claude"},
		{"qwen-max", true, "Qwen Max 支持 Claude"},
		{"qwen-turbo", true, "Qwen Turbo 支持 Claude"},
		{"qwen-coder", true, "Qwen Coder 支持 Claude"},
		{"qwen-vl-plus", true, "Qwen VL Plus 支持 Claude"},
		{"qwq-32b", true, "Qwq 思考模型支持 Claude"},
		{"qwen3.6-plus", true, "Qwen3.6 Plus 支持 Claude"},

		// GLM - 不支持 Claude 原生，需要 OpenAI 格式
		{"glm-5", false, "GLM-5 不支持 Claude 原生"},
		{"glm-4", false, "GLM-4 不支持 Claude 原生"},
		{"glm-3", false, "GLM-3 不支持 Claude 原生"},

		// 其他中国模型 - 不支持 Claude 原生
		{"moonshot-v1", false, "Moonshot 不支持 Claude 原生"},
		{"spark-3.5", false, "Spark 不支持 Claude 原生"},
		{"ernie-4", false, "Ernie 不支持 Claude 原生"},

		// 非中国模型
		{"gpt-4", false, "GPT-4 不是中国模型"},
		{"claude-sonnet-4-6", false, "Claude 不是中国模型"},
		{"gemini-pro", false, "Gemini 不是中国模型"},
	}

	for _, tt := range tests {
		t.Run(tt.modelName, func(t *testing.T) {
			result := isClaudeCompatibleChineseModel(tt.modelName)
			if result != tt.expected {
				t.Errorf("isClaudeCompatibleChineseModel(%q) = %v, want %v (%s)",
					tt.modelName, result, tt.expected, tt.desc)
			}
		})
	}
}

func TestIsOpenAICompatibleChineseModel(t *testing.T) {
	tests := []struct {
		modelName string
		expected  bool
		desc      string
	}{
		// GLM - 只支持 OpenAI
		{"glm-5", true, "GLM-5 只支持 OpenAI"},
		{"glm-4", true, "GLM-4 只支持 OpenAI"},

		// DeepSeek/MiniMax/Qwen - 支持 Claude，应该返回 false
		{"deepseek-v4", false, "DeepSeek 支持 Claude，不是 OpenAI 专用"},
		{"minimax-m2.7", false, "MiniMax 支持 Claude，不是 OpenAI 专用"},
		{"qwen-plus", false, "Qwen 支持 Claude，不是 OpenAI 专用"},

		// 其他中国模型
		{"moonshot-v1", true, "Moonshot 只支持 OpenAI"},
		{"spark-3.5", true, "Spark 只支持 OpenAI"},

		// 非中国模型
		{"gpt-4", false, "GPT-4 不是中国模型"},
	}

	for _, tt := range tests {
		t.Run(tt.modelName, func(t *testing.T) {
			result := isOpenAICompatibleChineseModel(tt.modelName)
			if result != tt.expected {
				t.Errorf("isOpenAICompatibleChineseModel(%q) = %v, want %v (%s)",
					tt.modelName, result, tt.expected, tt.desc)
			}
		})
	}
}

func TestIsRelayProvider(t *testing.T) {
	tests := []struct {
		providerType string
		expected     bool
		desc         string
	}{
		{"oneapi", true, "oneapi 是中转"},
		{"newapi", true, "newapi 是中转"},
		{"sub2api", true, "sub2api 是中转"},
		{"cliproxyapi", false, "cliproxyapi 不是这里判断的"},
		{"native", false, "native 不是中转"},
		{"", false, "空不是中转"},
		{"unknown", false, "unknown 不是中转"},
	}

	for _, tt := range tests {
		t.Run(tt.providerType, func(t *testing.T) {
			result := isRelayProvider(tt.providerType)
			if result != tt.expected {
				t.Errorf("isRelayProvider(%q) = %v, want %v (%s)",
					tt.providerType, result, tt.expected, tt.desc)
			}
		})
	}
}

func TestShouldSkipProbe(t *testing.T) {
	tests := []struct {
		providerType string
		expected     bool
		desc         string
	}{
		{"oneapi", false, "oneapi 不跳过探测"},
		{"newapi", false, "newapi 不跳过探测"},
		{"sub2api", false, "sub2api 不跳过探测"},
		{"cliproxyapi", true, "cliproxyapi 跳过探测"},
		{"native", false, "native 不跳过探测"},
		{"", false, "空不跳过探测"},
		{"windhub", true, "第三方中转跳过探测"},
		{"WINDHUB", true, "大写也应该跳过探测"},
		{"UnknownRelay", true, "未知的第三方中转跳过探测"},
	}

	for _, tt := range tests {
		t.Run(tt.providerType, func(t *testing.T) {
			result := shouldSkipProbe(tt.providerType)
			if result != tt.expected {
				t.Errorf("shouldSkipProbe(%q) = %v, want %v (%s)",
					tt.providerType, result, tt.expected, tt.desc)
			}
		})
	}
}

func TestInferAPIPathFromConfig(t *testing.T) {
	tests := []struct {
		providerType string
		transformer  string
		expectedPath string
		desc         string
	}{
		// oneapi/newapi 使用 openai transformer
		{"oneapi", "openai", "/v1/chat/completions", "oneapi openai"},
		{"oneapi", "auto", "/v1/chat/completions", "oneapi auto"},
		{"oneapi", "openai2", "/v1/responses", "oneapi openai2"},
		{"oneapi", "claude", "/v1/chat/completions", "oneapi claude"},
		{"oneapi", "gemini", "/v1beta/models/{model}:generateContent", "oneapi gemini"},

		// cliproxyapi 测试
		{"cliproxyapi", "openai", "/v1/chat/completions", "cliproxyapi openai"},
		{"cliproxyapi", "passthrough", "/v1/messages", "cliproxyapi passthrough"},
		{"cliproxyapi", "claude", "/v1/messages", "cliproxyapi claude"},

		// native 测试
		{"native", "openai", "/v1/chat/completions", "native openai"},
		{"native", "claude", "/v1/messages", "native claude"},

		// 空 provider 测试
		{"", "openai", "/v1/chat/completions", "空 openai"},
		{"", "claude", "/v1/messages", "空 claude"},
	}

	for _, tt := range tests {
		t.Run(tt.providerType+"_"+tt.transformer, func(t *testing.T) {
			result := inferAPIPathFromConfig(tt.providerType, tt.transformer)
			if result != tt.expectedPath {
				t.Errorf("inferAPIPathFromConfig(%q, %q) = %v, want %v (%s)",
					tt.providerType, tt.transformer, result, tt.expectedPath, tt.desc)
			}
		})
	}
}

func TestClassifyErrorType(t *testing.T) {
	tests := []struct {
		statusCode int
		errMsg     string
		expected   string
		desc       string
	}{
		{429, "rate limit exceeded", "rate_limit", "429 rate limit"},
		{429, "请求数限制", "rate_limit", "429 中文 rate limit"},
		{503, "Service temporarily unavailable", "service_unavailable", "503"},
		{502, "Bad Gateway", "bad_gateway", "502"},
		{504, "Gateway Timeout", "gateway_timeout", "504"},
		{404, "model not found", "model_not_found", "404 model"},
		{404, "Not Found", "not_found", "404 general"},
		{500, "Internal Server Error", "internal_error", "500"},
		{400, "Bad Request", "unknown", "400 unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			result := classifyErrorType(tt.statusCode, tt.errMsg)
			if result != tt.expected {
				t.Errorf("classifyErrorType(%d, %q) = %v, want %v (%s)",
					tt.statusCode, tt.errMsg, result, tt.expected, tt.desc)
			}
		})
	}
}

func TestGetErrorTypeLabel(t *testing.T) {
	tests := []struct {
		statusCode int
		errMsg     string
		expected   string
		desc       string
	}{
		{429, "rate limit", "速率限制 (429)", "429 label"},
		{503, "Service unavailable", "服务不可用 (503)", "503 label"},
		{404, "Not Found", "资源不存在 (404)", "404 label"},
		{404, "model not found", "模型不存在 (404)", "404 model label"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			result := getErrorTypeLabel(tt.statusCode, tt.errMsg)
			if result != tt.expected {
				t.Errorf("getErrorTypeLabel(%d, %q) = %v, want %v (%s)",
					tt.statusCode, tt.errMsg, result, tt.expected, tt.desc)
			}
		})
	}
}
