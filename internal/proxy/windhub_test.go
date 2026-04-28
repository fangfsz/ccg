package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type windhubConfig struct {
	baseURL string
	apiKey  string
}

func getWindhubConfig() (*windhubConfig, bool) {
	baseURL := os.Getenv("WINDHUB_BASE_URL")
	apiKey := os.Getenv("WINDHUB_API_KEY")
	if baseURL == "" || apiKey == "" {
		return nil, false
	}
	return &windhubConfig{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
	}, true
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int    `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func TestWindhub_ListModels(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	url := cfg.baseURL + "/v1/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Unexpected status code: %d, body: %s", resp.StatusCode, string(body))
	}

	var result modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	t.Logf("Found %d models:", len(result.Data))
	for _, m := range result.Data {
		t.Logf("  - %s (owned_by: %s)", m.ID, m.OwnedBy)
	}
}

func TestWindhub_ChatCompletions(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	testModels := []string{
		"glm-5.1",
		"glm-5.1-chat",
		"glm-5-chat",
		"deepseek-v3-250324",
		"deepseek-v3-2-251201",
		"grok-3",
		"grok-4",
		"kimi-k2.5",
	}

	url := cfg.baseURL + "/v1/chat/completions"

	for _, model := range testModels {
		t.Run(model, func(t *testing.T) {
			reqBody := chatRequest{
				Model: model,
				Messages: []chatMessage{
					{Role: "user", Content: "Say 'Hello, World!' in exactly 3 words."},
				},
				Stream:      false,
				Temperature: 0.7,
				MaxTokens:   50,
			}

			bodyBytes, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("Failed to marshal request: %v", err)
			}

			req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{Timeout: 60 * time.Second}
			start := time.Now()

			resp, err := client.Do(req)
			latency := time.Since(start)

			if err != nil {
				t.Errorf("Request failed: %v", err)
				return
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusOK {
				t.Errorf("Unexpected status: %d, body: %s, latency: %v", resp.StatusCode, string(respBody), latency)
				return
			}

			var result chatResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				t.Errorf("Failed to decode response: %v, body: %s", err, string(respBody))
				return
			}

			t.Logf("✓ %s - latency: %v, tokens: %d/%d/%d, response: %s",
				model,
				latency,
				result.Usage.PromptTokens,
				result.Usage.CompletionTokens,
				result.Usage.TotalTokens,
				result.Choices[0].Message.Content,
			)
		})
	}
}

func TestWindhub_ModelMapping(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	type modelTest struct {
		clientModel   string
		expectedModel string
		shouldSucceed bool
	}

	tests := []modelTest{
		{"gpt-4", "gpt-4", true},
		{"gpt-5.4", "glm-5.1", true},
		{"gpt-5.5", "glm-5.1", true},
		{"glm-5.1", "glm-5.1", true},
	}

	url := cfg.baseURL + "/v1/chat/completions"

	for _, test := range tests {
		t.Run(test.clientModel+"->"+test.expectedModel, func(t *testing.T) {
			reqBody := chatRequest{
				Model: test.clientModel,
				Messages: []chatMessage{
					{Role: "user", Content: "What model are you? Answer in 5 words or less."},
				},
				Stream:    false,
				MaxTokens: 30,
			}

			bodyBytes, _ := json.Marshal(reqBody)
			req, _ := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
			req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{Timeout: 60 * time.Second}
			start := time.Now()

			resp, err := client.Do(req)
			latency := time.Since(start)

			if err != nil {
				t.Errorf("Request failed: %v", err)
				return
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusOK && test.shouldSucceed {
				t.Errorf("Status: %d, body: %s, latency: %v", resp.StatusCode, string(respBody), latency)
				return
			}

			if resp.StatusCode == http.StatusOK {
				var result chatResponse
				if err := json.Unmarshal(respBody, &result); err != nil {
					t.Errorf("Decode error: %v", err)
					return
				}
				t.Logf("✓ %s -> %s (expected: %s), latency: %v, model response: %s",
					test.clientModel,
					result.Model,
					test.expectedModel,
					latency,
					result.Choices[0].Message.Content,
				)
			} else {
				t.Logf("✗ %s -> %s (expected: %s), status: %d, latency: %v",
					test.clientModel,
					"ERROR",
					test.expectedModel,
					resp.StatusCode,
					latency,
				)
			}
		})
	}
}

func TestWindhub_RequestSize(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	url := cfg.baseURL + "/v1/chat/completions"

	testSizes := []struct {
		name      string
		systemMsg string
	}{
		{"small", "You are a helpful assistant."},
		{"medium", strings.Repeat("This is a test system message. ", 100)},
		{"large", strings.Repeat("This is a test system message. ", 1000)},
	}

	for _, size := range testSizes {
		t.Run(size.name, func(t *testing.T) {
			reqBody := chatRequest{
				Model: "glm-4",
				Messages: []chatMessage{
					{Role: "system", Content: size.systemMsg},
					{Role: "user", Content: "Say 'OK' if you received my message."},
				},
				Stream:    false,
				MaxTokens: 10,
			}

			bodyBytes, _ := json.Marshal(reqBody)
			req, _ := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
			req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{Timeout: 60 * time.Second}
			start := time.Now()

			resp, err := client.Do(req)
			latency := time.Since(start)

			if err != nil {
				t.Errorf("Request failed: %v", err)
				return
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)

			t.Logf("%s: request size=%d bytes, status=%d, latency=%v",
				size.name,
				len(bodyBytes),
				resp.StatusCode,
				latency,
			)

			if resp.StatusCode != http.StatusOK {
				t.Errorf("Request failed: %s", string(respBody))
			}
		})
	}
}

func TestWindhub_Streaming(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	url := cfg.baseURL + "/v1/chat/completions"

	reqBody := chatRequest{
		Model: "glm-4",
		Messages: []chatMessage{
			{Role: "user", Content: "Count from 1 to 5, one number per line."},
		},
		Stream:    true,
		MaxTokens: 50,
	}

	bodyBytes, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 60 * time.Second}
	start := time.Now()

	resp, err := client.Do(req)
	latency := time.Since(start)

	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Unexpected status: %d, body: %s", resp.StatusCode, string(body))
	}

	var fullContent strings.Builder
	reader := resp.Body
	buffer := make([]byte, 1024)
	eventName := ""
	eventData := ""

	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			line := string(buffer[:n])
			lines := strings.Split(line, "\n")
			for _, l := range lines {
				l = strings.TrimSuffix(l, "\r")
				if strings.HasPrefix(l, "event:") {
					eventName = strings.TrimSpace(strings.TrimPrefix(l, "event:"))
				} else if strings.HasPrefix(l, "data:") {
					eventData = strings.TrimSpace(strings.TrimPrefix(l, "data:"))
					if eventData == "[DONE]" {
						break
					}
					if eventName == "content_delta" {
						fullContent.WriteString(eventData)
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Errorf("Read error: %v", err)
			break
		}
	}

	t.Logf("✓ Streaming completed in %v, content: %s", latency, fullContent.String())
}

func TestWindhub_InferAPIPath(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	paths := []string{
		"/v1/chat/completions",
		"/v1/models",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			url := cfg.baseURL + path
			method := "GET"
			if path == "/v1/chat/completions" {
				method = "POST"
			}

			var req *http.Request
			var err error

			if method == "POST" {
				reqBody := chatRequest{
					Model: "glm-4",
					Messages: []chatMessage{
						{Role: "user", Content: "Hi"},
					},
					Stream:    false,
					MaxTokens: 5,
				}
				bodyBytes, _ := json.Marshal(reqBody)
				req, err = http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req, err = http.NewRequest("GET", url, nil)
			}

			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+cfg.apiKey)

			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)

			if err != nil {
				t.Errorf("Request failed: %v", err)
				return
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			t.Logf("Path %s: status=%d", path, resp.StatusCode)

			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
				t.Logf("✓ Path %s is valid (status %d)", path, resp.StatusCode)
			} else {
				t.Logf("✗ Path %s returned status %d, body: %s", path, resp.StatusCode, string(respBody))
			}
		})
	}
}

func TestWindhub_TransformerCompatibility(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	url := cfg.baseURL + "/v1/chat/completions"

	transformers := []struct {
		name    string
		payload string
	}{
		{
			"openai",
			`{"model":"glm-4","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}`,
		},
		{
			"openai2",
			`{"model":"glm-4","input":"Hi","max_tokens":5}`,
		},
		{
			"claude",
			`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}`,
		},
	}

	for _, tr := range transformers {
		t.Run(tr.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", url, strings.NewReader(tr.payload))
			req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)

			if err != nil {
				t.Errorf("Request failed: %v", err)
				return
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			t.Logf("%s: status=%d, response=%s", tr.name, resp.StatusCode, string(body))

			if resp.StatusCode == http.StatusOK {
				t.Logf("✓ %s transformer compatible", tr.name)
			} else {
				t.Logf("✗ %s transformer NOT compatible (status %d)", tr.name, resp.StatusCode)
			}
		})
	}
}

func TestWindhub_TimeoutBehavior(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	url := cfg.baseURL + "/v1/chat/completions"

	timeoutValues := []time.Duration{
		5 * time.Second,
		15 * time.Second,
		30 * time.Second,
		60 * time.Second,
	}

	for _, timeout := range timeoutValues {
		t.Run(fmt.Sprintf("%v", timeout), func(t *testing.T) {
			reqBody := chatRequest{
				Model: "glm-4",
				Messages: []chatMessage{
					{Role: "user", Content: "Count from 1 to 100, one number per line."},
				},
				Stream:    false,
				MaxTokens: 300,
			}

			bodyBytes, _ := json.Marshal(reqBody)
			req, _ := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
			req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{Timeout: timeout}
			start := time.Now()

			resp, err := client.Do(req)
			latency := time.Since(start)

			if err != nil {
				if strings.Contains(err.Error(), "timeout") {
					t.Logf("✗ Timeout after %v (timeout setting: %v)", latency, timeout)
				} else {
					t.Errorf("Request failed: %v", err)
				}
				return
			}
			defer resp.Body.Close()

			t.Logf("✓ Completed in %v (timeout setting: %v, status: %d)", latency, timeout, resp.StatusCode)
		})
	}
}

func TestWindhub_CompareRequestMethods(t *testing.T) {
	cfg, ok := getWindhubConfig()
	if !ok {
		t.Skip("WINDHUB_BASE_URL or WINDHUB_API_KEY not set, skipping integration test")
	}

	url := cfg.baseURL + "/v1/chat/completions"

	reqBody := chatRequest{
		Model: "glm-4",
		Messages: []chatMessage{
			{Role: "user", Content: "Say 'OK'"},
		},
		Stream:    false,
		MaxTokens: 5,
	}

	bodyBytes, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 30 * time.Second}

	t.Run("POST", func(t *testing.T) {
		req, _ := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
		req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, err := client.Do(req)
		latency := time.Since(start)

		if err != nil {
			t.Errorf("POST failed: %v", err)
			return
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		t.Logf("POST: status=%d, latency=%v, body=%s", resp.StatusCode, latency, string(body))
	})

	t.Run("GET", func(t *testing.T) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, err := client.Do(req)
		latency := time.Since(start)

		if err != nil {
			t.Errorf("GET failed: %v", err)
			return
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		t.Logf("GET: status=%d, latency=%v, body=%s", resp.StatusCode, latency, string(body))
	})
}
