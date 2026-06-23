package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unsafe"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// OpenAICompatibleClient 是一个基于原生 net/http 的通用 OpenAI 兼容协议客户端。
type OpenAICompatibleClient struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// OpenAIRequest 表示一个发往 OpenAI 兼容接口的请求载荷。
type OpenAIRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	Stream      bool            `json:"stream"`
	// 可选字段
	ResponseFormat *OpenAIResponseFormat `json:"response_format,omitempty"`
	Tools          []OpenAITool          `json:"tools,omitempty"`
	StreamOptions  *OpenAIStreamOptions  `json:"stream_options,omitempty"`
	// DeepSeek thinking mode 控制
	// ReasoningEffort: "high" | "max"（ThinkingDisabled 时不发送）
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Thinking        *ThinkingConfig `json:"thinking,omitempty"`
}

// ThinkingConfig DeepSeek extended thinking 控制体。
// Type 固定为 "enabled"；BudgetTokens 可选（0 = 不限制）。
type ThinkingConfig struct {
	Type         string `json:"type"` // 固定 "enabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// OpenAIStreamOptions 控制流式响应附加行为。
// IncludeUsage=true 时，API 在最后一个 chunk 中返回完整 usage（prompt+completion tokens）。
type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
}

type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

type OpenAITool struct {
	Type     string               `json:"type"`
	Function OpenAIToolDefinition `json:"function"`
}

type OpenAIToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// OpenAIResponse 表示一个完整的非流式响应。
type OpenAIResponse struct {
	ID      string         `json:"id"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// SendRequest 发送一个非流式的 HTTP 请求。
func (c *OpenAICompatibleClient) SendRequest(ctx context.Context, apiKey []byte, req *OpenAIRequest) (*OpenAIResponse, error) {
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to marshal request", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to create request", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	cleanup := setAuthHeader(httpReq, apiKey)
	defer cleanup()

	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "http request failed", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20))
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("api error (status %d): %s", httpResp.StatusCode, strings.TrimSpace(string(body))))
	}

	var resp OpenAIResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to decode response", err)
	}

	return &resp, nil
}

// translateRequest 将内部的 types.InferRequest 转换为 OpenAI 兼容的载荷。
func translateRequest(req *types.InferRequest, supportsVision bool) *OpenAIRequest {
	out := &OpenAIRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Stream:    false,
	}

	// ThinkingMode 路由：enabled 时强制 Temperature=0，DeepSeek 要求
	switch req.ThinkingMode {
	case types.ThinkingHigh:
		out.ReasoningEffort = "high"
		out.Thinking = &ThinkingConfig{Type: "enabled"}
		out.Temperature = 0 // thinking 模式不兼容 temperature/top_p
	case types.ThinkingMax:
		out.ReasoningEffort = "max"
		out.Thinking = &ThinkingConfig{Type: "enabled"}
		out.Temperature = 0
	default:
		// ThinkingDisabled：不发送 thinking 字段，透传用户 temperature
		out.Temperature = req.Temperature
	}

	if req.ResponseFormat != nil {
		if req.ResponseFormat.Type == "json_schema" {
			out.ResponseFormat = &OpenAIResponseFormat{
				Type:       "json_schema",
				JSONSchema: map[string]any{"name": "structured_output", "strict": true, "schema": req.ResponseFormat.JSONSchema},
			}
		} else {
			out.ResponseFormat = &OpenAIResponseFormat{Type: req.ResponseFormat.Type}
		}
	}

	for _, msg := range req.Messages {
		if len(msg.Parts) > 0 {
			oaiMsgs := partsToOpenAIMessages(msg.Role, msg.Parts, supportsVision)
			// DeepSeek thinking mode：assistant 消息必须携带 reasoning_content 回传
			if msg.ReasoningContent != "" && len(oaiMsgs) > 0 && oaiMsgs[0].Role == "assistant" {
				oaiMsgs[0].ReasoningContent = msg.ReasoningContent
			}
			out.Messages = append(out.Messages, oaiMsgs...)
		} else {
			// DeepSeek thinking mode：纯文本 assistant 消息也必须回传 reasoning_content
			oaiMsg := OpenAIMessage{
				Role:             msg.Role,
				Content:          msg.Content,
				ReasoningContent: msg.ReasoningContent, // 空字符串时 json omitempty 自动省略
			}
			out.Messages = append(out.Messages, oaiMsg)
		}
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	return out
}

// partsToOpenAIMessages 将 types.Message.Parts（Anthropic 多块格式）转换为 OpenAI message 列表。
// assistant Parts → 单条 assistant message（含 tool_calls）
// user Parts     → 多条 role=tool message（每个 tool_result 一条）
func partsToOpenAIMessages(role string, parts []any, supportsVision bool) []OpenAIMessage {
	if role == "assistant" {
		return parseAssistantParts(parts)
	}
	if role == "user" {
		return parseUserParts(parts, supportsVision)
	}
	return nil
}

func parseAssistantParts(parts []any) []OpenAIMessage {
	var textContent string
	var toolCalls []OpenAIToolCall
	for _, p := range parts {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			textContent, _ = m["text"].(string)
		case "tool_use":
			toolCalls = append(toolCalls, parseToolUsePart(m))
		}
	}
	return []OpenAIMessage{{Role: "assistant", Content: textContent, ToolCalls: toolCalls}}
}

func parseToolUsePart(m map[string]any) OpenAIToolCall {
	id, _ := m["id"].(string)
	name, _ := m["name"].(string)
	var argsStr string
	switch v := m["input"].(type) {
	case json.RawMessage:
		argsStr = string(v)
	case string:
		argsStr = v
	default:
		b, _ := json.Marshal(v)
		argsStr = string(b)
	}
	if argsStr == "" {
		argsStr = "{}"
	}
	return OpenAIToolCall{
		ID:       id,
		Type:     "function",
		Function: OpenAIFunctionCall{Name: name, Arguments: argsStr},
	}
}

func parseUserParts(parts []any, supportsVision bool) []OpenAIMessage {
	var msgs []OpenAIMessage
	var contentBlocks []any
	for _, p := range parts {
		if ip, ok := p.(types.ImagePart); ok {
			if supportsVision {
				contentBlocks = append(contentBlocks, parseImagePart(ip))
			} else {
				// Fallback text if vision not supported to prevent deserialization errors on the provider end
				contentBlocks = append(contentBlocks, map[string]any{
					"type": "text",
					"text": "[Image Input: Model does not support vision]",
				})
			}
			continue
		}

		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "text" {
			if txt, ok := m["text"].(string); ok {
				contentBlocks = append(contentBlocks, map[string]any{
					"type": "text",
					"text": txt,
				})
			}
		}
		if m["type"] == "tool_result" {
			toolCallID, _ := m["tool_use_id"].(string)
			content, _ := m["content"].(string)
			msgs = append(msgs, OpenAIMessage{
				Role:       "tool",
				ToolCallID: toolCallID,
				Content:    content,
			})
		}
	}
	if len(contentBlocks) > 0 {
		msgs = append(msgs, OpenAIMessage{
			Role:    "user",
			Content: contentBlocks,
		})
	}
	return msgs
}

func parseImagePart(ip types.ImagePart) map[string]any {
	if ip.URL != "" {
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": ip.URL},
		}
	}
	return map[string]any{
		"type": "image_url",
		"image_url": map[string]string{
			"url": "data:" + ip.MediaType + ";base64," + base64.StdEncoding.EncodeToString(ip.Data),
		},
	}
}

// setAuthHeader 将 "Bearer <key>" 写入 Authorization header。
// 使用 unsafe.String 避免额外堆分配；返回清理函数供请求完成后清零。
func setAuthHeader(req *http.Request, key []byte) func() {
	const prefix = "Bearer "
	bearer := make([]byte, len(prefix)+len(key))
	copy(bearer, prefix)
	copy(bearer[len(prefix):], key)
	req.Header.Set("Authorization", unsafe.String(unsafe.SliceData(bearer), len(bearer)))
	return func() {
		req.Header.Del("Authorization") // 删除 Header map 引用（inv_M1_06）
		for i := range bearer {
			bearer[i] = 0
		}
	}
}
