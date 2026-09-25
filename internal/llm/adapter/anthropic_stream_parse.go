package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

// Anthropic SSE 事件流解析（2026-09-22 自 anthropic_request.go 拆出：该文件因
// 补齐 scanner.Err()/空响应体检测后达到 411 行，超过 R7 的 400 行上限；拆分边界
// 取"构造请求"与"解析响应"两个独立职责，与 internal/llm 的 router.go /
// router_stream.go 拆法一致，逻辑未作任何改动）。

// emitToolCallOnBlockStop 在 content_block_stop 事件到达且当前处于 tool_use block 内时，
// 拼装/修复累积的 partial_json 并发送 StreamToolCall 事件（从 parseAnthropicStream 拆出，
// nestif 治理，行为不变）。返回 ctxDone=true 时调用方应立即从 parseAnthropicStream return。
func (a *AnthropicAdapter) emitToolCallOnBlockStop(ctx context.Context, ch chan<- types.StreamEvent, toolID, toolName string, toolInputBuf *strings.Builder) (ctxDone bool) {
	inputJSON := toolInputBuf.String()
	if inputJSON == "" {
		inputJSON = "{}"
	} else if !json.Valid([]byte(inputJSON)) {
		// 流被截断导致 tool_use 的 input_json_delta 拼接结果不是合法
		// JSON：json.RawMessage 校验会使下方 json.Marshal 静默失败
		// （payload=nil），StreamToolCall 事件内容凭空丢失。用
		// JSONRepair 栈式修复尽力抢救已收到的参数片段。
		if repaired, repairErr := llmparent.JSONRepair([]byte(inputJSON)); repairErr == nil && json.Valid(repaired.Repaired) {
			inputJSON = string(repaired.Repaired)
		} else {
			inputJSON = "{}"
		}
	}
	payload, err := json.Marshal(map[string]any{
		"id":    toolID,
		"name":  toolName,
		"input": json.RawMessage(inputJSON),
	})
	if err != nil {
		select {
		case ch <- types.StreamEvent{Type: types.StreamError, Content: fmt.Sprintf("tool_call payload marshal failed: %v", err)}:
		case <-ctx.Done():
			return true
		}
		return false
	}
	select {
	case ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}:
	case <-ctx.Done():
		return true
	}
	return false
}

// parseAnthropicStream 解析 Anthropic SSE 事件并转换为统一的 StreamEvent。
// tool_use 事件打包为 StreamToolCall，Content 为 JSON: {"id","name","input"}。
func (a *AnthropicAdapter) parseAnthropicStream(ctx context.Context, model string, body io.Reader, ch chan<- types.StreamEvent) { //nolint:gocyclo
	scanner := bufio.NewScanner(body)
	var toolID, toolName string
	var toolInputBuf strings.Builder
	inToolBlock := false
	inThinkingBlock := false // 标记当前是否在处理 extended thinking block
	// parsedAny 是否解析出过至少一帧合法 SSE data。message_stop 在循环内 return，
	// 故末尾为 false 仅意味着 200 响应体里一帧合法数据都没有（HTML/文本错误页）。
	parsedAny := false

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var frame struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"` // extended thinking delta
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			continue
		}
		parsedAny = true

		switch frame.Type {
		case "message_start":
			if frame.Message.Usage.InputTokens > 0 {
				select {
				case ch <- types.StreamEvent{
					Type: types.StreamTextDelta,
					Usage: types.Usage{
						InputTokens:         frame.Message.Usage.InputTokens,
						CacheHitTokens:      frame.Message.Usage.CacheReadInputTokens,
						CacheCreationTokens: frame.Message.Usage.CacheCreationInputTokens,
					},
				}:
				case <-ctx.Done():
					return
				}
				if frame.Message.Usage.InputTokens > 0 {
					if a.tbr != nil {
						a.tbr.Add(int64(frame.Message.Usage.InputTokens))
					}
				}
				hit := frame.Message.Usage.CacheReadInputTokens > 0
				metrics.RecordLLMCacheHit("anthropic", model, hit)
			}
		case "content_block_start":
			switch frame.ContentBlock.Type {
			case "tool_use":
				toolID = frame.ContentBlock.ID
				toolName = frame.ContentBlock.Name
				toolInputBuf.Reset()
				inToolBlock = true
				inThinkingBlock = false
			case "thinking":
				// Anthropic extended thinking block 开始
				inThinkingBlock = true
				inToolBlock = false
			default:
				inToolBlock = false
				inThinkingBlock = false
			}
		case "content_block_delta":
			switch {
			case inToolBlock && frame.Delta.Type == "input_json_delta":
				toolInputBuf.WriteString(frame.Delta.PartialJSON)
			case inThinkingBlock && frame.Delta.Type == "thinking_delta" && frame.Delta.Thinking != "":
				// 将思考内容发送给前端显示
				select {
				case ch <- types.StreamEvent{Type: types.StreamThinking, Content: frame.Delta.Thinking}:
				case <-ctx.Done():
					return
				}
			case !inToolBlock && !inThinkingBlock && frame.Delta.Type == "text_delta" && frame.Delta.Text != "":
				select {
				case ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: frame.Delta.Text}:
				case <-ctx.Done():
					return
				}
			}
		case "content_block_stop":
			if inToolBlock {
				if a.emitToolCallOnBlockStop(ctx, ch, toolID, toolName, &toolInputBuf) {
					return
				}
			}
			inToolBlock = false
			inThinkingBlock = false
		case "message_delta":
			if frame.Usage.OutputTokens > 0 {
				select {
				case ch <- types.StreamEvent{
					Type:  types.StreamTextDelta,
					Usage: types.Usage{OutputTokens: frame.Usage.OutputTokens},
				}:
				case <-ctx.Done():
					return
				}
				if a.tbr != nil {
					a.tbr.Add(int64(frame.Usage.OutputTokens))
				}
			}
		case "message_stop":
			return
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		select {
		case ch <- types.StreamEvent{Type: types.StreamError, Content: fmt.Sprintf("stream read: %v", err)}:
		case <-ctx.Done():
		}
		return
	}

	if !parsedAny {
		select {
		case ch <- types.StreamEvent{Type: types.StreamError, Content: "provider returned a 200 response with no valid SSE data frame"}:
		case <-ctx.Done():
		}
	}
}
