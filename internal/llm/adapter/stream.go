package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// OpenAIStreamDelta 流式响应中的 delta 字段（支持文本和 tool_call 两类）。
type OpenAIStreamDelta struct {
	Content          string                `json:"content"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls,omitempty"`
}

type openAIFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIToolCallDelta struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function openAIFunctionDelta `json:"function"`
}

// OpenAIStreamChunk 表示流式响应的每个增量数据块。
type OpenAIStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int               `json:"index"`
		Delta        OpenAIStreamDelta `json:"delta"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage *OpenAIUsage `json:"usage,omitempty"`
}

// SendStreamRequest 发送一个流式的 HTTP 请求并返回解析事件的 channel。
// estimatedInputTokens 由调用方通过 MultimodalTokenizer.EstimateRequest 预估；
// 当 ctx 被取消时，StreamCancelled 事件用该值作为 InputTokens 补偿计费。
func (c *OpenAICompatibleClient) SendStreamRequest(ctx context.Context, apiKey []byte, req *OpenAIRequest, estimatedInputTokens int) (<-chan types.StreamEvent, error) { //nolint:gocyclo
	req.Stream = true
	// 要求 API 在最后一个 chunk 附带完整 usage，供精确计费
	req.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}

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
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "http request failed", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20))
		httpResp.Body.Close()
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("api error (status %d): %s", httpResp.StatusCode, strings.TrimSpace(string(body))))
	}

	ch := make(chan types.StreamEvent, 64)

	go func() {
		defer httpResp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(httpResp.Body)
		buf := make([]byte, 4096)
		scanner.Buffer(buf, 1024*1024)

		// 跨 chunk 聚合 tool_call 参数：index → 状态
		type toolCallState struct {
			id        string
			name      string
			arguments strings.Builder
		}
		toolBuilders := map[int]*toolCallState{}
		accumulatedOutputTokens := 0

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				// 用户主动取消：发出补偿计费事件后退出。
				// InputTokens 用预估值（完整请求已发给 API），OutputTokens 为已收到的实际数量。
				ch <- types.StreamEvent{
					Type: types.StreamCancelled,
					Usage: types.Usage{
						InputTokens:  estimatedInputTokens,
						OutputTokens: accumulatedOutputTokens,
					},
				}
				return
			default:
			}

			line := scanner.Text()
			if len(strings.TrimSpace(line)) == 0 {
				continue
			}

			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			if data == "[DONE]" {
				return
			}

			var chunk OpenAIStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				ch <- types.StreamEvent{Type: types.StreamError, Content: fmt.Sprintf("parse chunk: %v", err)}
				return
			}

			var currentUsage types.Usage
			if chunk.Usage != nil {
				currentUsage = types.Usage{
					InputTokens:  chunk.Usage.PromptTokens,
					OutputTokens: chunk.Usage.CompletionTokens,
				}
				if chunk.Usage.PromptTokensDetails != nil {
					currentUsage.CacheHitTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				}
				// API 返回的精确值优先；更新累计输出 token，供后续 cancel 补偿用
				accumulatedOutputTokens = chunk.Usage.CompletionTokens
			}

			if len(chunk.Choices) == 0 {
				if chunk.Usage != nil {
					ch <- types.StreamEvent{Type: types.StreamTextDelta, Usage: currentUsage}
				}
				continue
			}

			choice := chunk.Choices[0]
			delta := choice.Delta

			// 思考链 delta（DeepSeek thinking mode）
			if delta.ReasoningContent != "" {
				ch <- types.StreamEvent{Type: types.StreamThinking, Content: delta.ReasoningContent}
			}

			// 文本 delta
			if delta.Content != "" {
				// 无精确 usage 时累计字符估算，供 cancel 补偿参考
				if chunk.Usage == nil {
					accumulatedOutputTokens += len([]rune(delta.Content)) / 3
				}
				ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: delta.Content, Usage: currentUsage}
			}

			// tool_call delta：拼接参数
			for _, tc := range delta.ToolCalls {
				s, exists := toolBuilders[tc.Index]
				if !exists {
					s = &toolCallState{}
					toolBuilders[tc.Index] = s
				}
				if tc.ID != "" {
					s.id = tc.ID
				}
				if tc.Function.Name != "" {
					s.name = tc.Function.Name
				}
				s.arguments.WriteString(tc.Function.Arguments)
			}

			// finish_reason == "tool_calls" → 把所有已收集的 tool_call emit 出去
			if choice.FinishReason == "tool_calls" {
				for idx := range len(toolBuilders) {
					s, ok := toolBuilders[idx]
					if !ok {
						continue
					}
					argsStr := s.arguments.String()
					if argsStr == "" {
						argsStr = "{}"
					}
					payload, _ := json.Marshal(map[string]any{
						"id":    s.id,
						"name":  s.name,
						"input": json.RawMessage(argsStr),
					})
					ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
				}
				// 清空，支持同一流中多轮（理论上不会，但防御性清空）
				toolBuilders = map[int]*toolCallState{}
			}
		}

		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			ch <- types.StreamEvent{Type: types.StreamError, Content: fmt.Sprintf("stream read: %v", err)}
		}
	}()

	return ch, nil
}
