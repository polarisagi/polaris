package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/pkg/types"
)

type MockTransport struct {
	RoundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *MockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.RoundTripFunc(req)
}

func TestAdapters_Infer(t *testing.T) {
	credPool := llmparent.NewCredentialPool([]string{"test-key"}, llmparent.StrategyFillFirst)
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

		adapter := NewOpenAIAdapter("https://api.openai.com/v1", "gpt-4-turbo", credPool, client, nil)
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

		adapter := NewAnthropicAdapter("claude-3-opus-20240229", credPool, client, nil)
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

		adapter := NewDeepSeekAdapter(credPool, client, "deepseek-reasoner", nil)
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

		adapter := NewGoogleAgentPlatformAdapter("gemini-1.5-pro", "", "", credPool, client, nil)
		resp, err := adapter.Infer(context.Background(), msgs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Content != "Gemini hi" {
			t.Fatalf("bad content: %s", resp.Content)
		}
	})
}

// TestOpenAIAdapter_ModelResolution 锁定"模型 ID 归属 Provider 配置"这条契约：
// req.Model 为空时必须用 adapter 自身配置的模型（由 provider_models 表按 role
// 注入），非空时才覆盖。
//
// 2026-09-22 回归背景：internal/agent 曾用 types.WithModel(llmEff.ModelPool) 把
// **角色池名**（general/reasoning/...）塞进 req.Model，覆盖掉这里配好的真实模型
// ID，导致发往 DeepSeek 的请求 model 字段字面是 "general" 并被 400 拒绝
// （"The supported API model names are ..., but you passed general."）。该错误
// 此前被 Agent 侧静默吞掉，只在用户侧表现为"推理返回空内容"。
func TestOpenAIAdapter_ModelResolution(t *testing.T) {
	credPool := llmparent.NewCredentialPool([]string{"test-key"}, llmparent.StrategyFillFirst)
	msgs := []types.Message{{Role: "user", Content: "Hi"}}

	mockResp := map[string]interface{}{
		"choices": []map[string]interface{}{
			{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": "ok"}, "finish_reason": "stop"},
		},
	}
	bodyBytes, _ := json.Marshal(mockResp)

	for _, tc := range []struct {
		name      string
		reqModel  string
		wantModel string
	}{
		{"空 Model 用 Provider 配置", "", "deepseek-v4-pro"},
		{"显式 Model 覆盖", "deepseek-flash", "deepseek-flash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sentModel string
			client := &http.Client{Transport: &MockTransport{
				RoundTripFunc: func(req *http.Request) (*http.Response, error) {
					var payload struct {
						Model string `json:"model"`
					}
					raw, _ := io.ReadAll(req.Body)
					_ = json.Unmarshal(raw, &payload)
					sentModel = payload.Model
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(bytes.NewBuffer(bodyBytes)),
						Header:     make(http.Header),
					}, nil
				},
			}}

			adapter := NewOpenAIAdapter("https://api.deepseek.com", "deepseek-v4-pro", credPool, client, nil)
			var opts []types.InferOption
			if tc.reqModel != "" {
				opts = append(opts, types.WithModel(tc.reqModel))
			}
			if _, err := adapter.Infer(context.Background(), msgs, opts...); err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if sentModel != tc.wantModel {
				t.Errorf("model sent to API = %q, want %q", sentModel, tc.wantModel)
			}
		})
	}
}

// TestDisableDeepSeekThinking DeepSeek 省略 thinking 字段即默认开启思考，
// 显式 ThinkingDisabled 必须落为 thinking.type=disabled；未指定保持服务端默认。
func TestDisableDeepSeekThinking(t *testing.T) {
	req := translateRequest(&types.InferRequest{ThinkingMode: types.ThinkingDisabled}, false)
	disableDeepSeekThinking(req, types.ThinkingDisabled)
	if req.Thinking == nil || req.Thinking.Type != "disabled" {
		t.Fatalf("显式 ThinkingDisabled 应发送 thinking.type=disabled，got %+v", req.Thinking)
	}

	unset := translateRequest(&types.InferRequest{}, false)
	disableDeepSeekThinking(unset, "")
	if unset.Thinking != nil {
		t.Fatalf("未指定思考模式不应改写请求，got %+v", unset.Thinking)
	}

	maxReq := translateRequest(&types.InferRequest{ThinkingMode: types.ThinkingMax}, false)
	disableDeepSeekThinking(maxReq, types.ThinkingMax)
	if maxReq.Thinking == nil || maxReq.Thinking.Type != "enabled" {
		t.Fatalf("ThinkingMax 不应被改写，got %+v", maxReq.Thinking)
	}
}
