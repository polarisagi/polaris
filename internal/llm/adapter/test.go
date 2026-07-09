package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

type MockTransport struct {
	RoundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *MockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.RoundTripFunc(req)
}

func TestAdapters_Infer(t *testing.T) {
	credFn := func() []byte { return []byte("test-key") }
	msgs := []types.Message{
		{Role: "user", Content: "Hi"},
	}

	t.Run("OpenAI", func(t *testing.T) {
		mockResp := map[string]interface{}{
			"id":      "chatcmpl-123",
			"object":  "chat.completion",
			"created": 1677652288,
			"model":   "gpt-4-turbo",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Hello there",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     9,
				"completion_tokens": 12,
				"total_tokens":      21,
			},
		}
		bodyBytes, _ := json.Marshal(mockResp)
		client := &http.Client{
			Transport: &MockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(bytes.NewBuffer(bodyBytes)),
						Header:     make(http.Header),
					}, nil
				},
			},
		}

		adapter := NewOpenAIAdapter("https://api.openai.com/v1", "gpt-4-turbo", credFn, client, nil)
		if adapter.ModelID() != "gpt-4-turbo" {
			t.Errorf("expected gpt-4-turbo, got %s", adapter.ModelID())
		}

		resp, err := adapter.Infer(context.Background(), msgs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Content != "Hello there" {
			t.Fatalf("bad content: %s", resp.Content)
		}
	})

	t.Run("Anthropic", func(t *testing.T) {
		mockResp := map[string]interface{}{
			"id":    "msg_123",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-3-opus-20240229",
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": "Hello from Claude",
				},
			},
			"usage": map[string]interface{}{
				"input_tokens":  15,
				"output_tokens": 10,
			},
		}
		bodyBytes, _ := json.Marshal(mockResp)
		client := &http.Client{
			Transport: &MockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(bytes.NewBuffer(bodyBytes)),
						Header:     make(http.Header),
					}, nil
				},
			},
		}

		adapter := NewAnthropicAdapter("claude-3-opus-20240229", credFn, client, nil)
		resp, err := adapter.Infer(context.Background(), msgs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Content != "Hello from Claude" {
			t.Fatalf("bad content: %s", resp.Content)
		}
	})

	t.Run("DeepSeek", func(t *testing.T) {
		mockResp := map[string]interface{}{
			"id":      "chatcmpl-123",
			"object":  "chat.completion",
			"created": 1677652288,
			"model":   "deepseek-reasoner",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":              "assistant",
						"content":           "DeepSeek says hi",
						"reasoning_content": "DeepSeek is reasoning",
					},
					"finish_reason": "stop",
				},
			},
		}
		bodyBytes, _ := json.Marshal(mockResp)
		client := &http.Client{
			Transport: &MockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(bytes.NewBuffer(bodyBytes)),
						Header:     make(http.Header),
					}, nil
				},
			},
		}

		adapter := NewDeepSeekAdapter(credFn, client, "deepseek-reasoner", nil)
		resp, err := adapter.Infer(context.Background(), msgs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Content != "DeepSeek says hi" {
			t.Fatalf("bad content: %s", resp.Content)
		}
		if resp.ReasoningContent != "DeepSeek is reasoning" {
			t.Fatalf("bad reasoning content: %s", resp.ReasoningContent)
		}
	})

	t.Run("Ollama", func(t *testing.T) {
		mockResp := map[string]interface{}{
			"id":      "chatcmpl-123",
			"object":  "chat.completion",
			"created": 1677652288,
			"model":   "llama3",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Ollama hello",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     9,
				"completion_tokens": 12,
				"total_tokens":      21,
			},
		}
		bodyBytes, _ := json.Marshal(mockResp)
		client := &http.Client{
			Transport: &MockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(bytes.NewBuffer(bodyBytes)),
						Header:     make(http.Header),
					}, nil
				},
			},
		}

		adapter := NewOllamaAdapter("llama3", client, nil)
		resp, err := adapter.Infer(context.Background(), msgs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Content != "Ollama hello" {
			t.Fatalf("bad content: %s", resp.Content)
		}
	})

	t.Run("Google", func(t *testing.T) {
		mockResp := map[string]interface{}{
			"candidates": []map[string]interface{}{
				{
					"content": map[string]interface{}{
						"role": "model",
						"parts": []map[string]interface{}{
							{
								"text": "Gemini hi",
							},
						},
					},
					"finishReason": "STOP",
				},
			},
		}
		bodyBytes, _ := json.Marshal(mockResp)
		client := &http.Client{
			Transport: &MockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(bytes.NewBuffer(bodyBytes)),
						Header:     make(http.Header),
					}, nil
				},
			},
		}

		adapter := NewGoogleAgentPlatformAdapter("gemini-1.5-pro", "", "", credFn, client, nil)
		resp, err := adapter.Infer(context.Background(), msgs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Content != "Gemini hi" {
			t.Fatalf("bad content: %s", resp.Content)
		}
	})
}
