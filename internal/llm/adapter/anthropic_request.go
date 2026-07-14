package adapter

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unsafe"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	llmparent "github.com/polarisagi/polaris/internal/llm"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func (a *AnthropicAdapter) buildAnthropicRequest(req *types.InferRequest, stream bool) ([]byte, error) { //nolint:gocyclo
	model := resolveAnthropicModel(a.model)
	if req.Model != "" {
		model = resolveAnthropicModel(req.Model)
	}

	// 转换 messages
	var msgs []map[string]any
	var system string
	for _, m := range req.Messages {
		if m.Role == "system" {
			system += m.Content + "\n"
			continue
		}
		if len(m.Parts) > 0 {
			var contentBlocks []any
			for _, p := range m.Parts {
				switch v := p.(type) {
				case types.ImagePart:
					contentBlocks = append(contentBlocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": v.MediaType,
							"data":       base64.StdEncoding.EncodeToString(v.Data),
						},
					})
				default:
					contentBlocks = append(contentBlocks, v)
				}
			}
			msgs = append(msgs, map[string]any{"role": m.Role, "content": contentBlocks})
		} else {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
		}
	}

	payload := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": req.MaxTokens,
	}
	if system != "" {
		payload["system"] = strings.TrimSpace(system)
	}
	if req.MaxTokens <= 0 {
		payload["max_tokens"] = 4096
	}
	if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if stream {
		payload["stream"] = true
	}

	if req.ThinkingMode != "" && req.ThinkingMode != types.ThinkingDisabled {
		budget := req.ThinkingBudget
		if budget <= 0 {
			budget = 8000
		}
		payload["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": budget,
		}
	}

	// 传入工具 schema（Anthropic tools 格式）
	if len(req.Tools) > 0 {
		anthropicTools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			anthropicTools = append(anthropicTools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": schema,
			})
		}
		payload["tools"] = anthropicTools
	}

	// Anthropic Prompt Caching：system_and_3 策略，最多 4 个断点。
	// 断点 1: system prompt（跨会话稳定，命中率最高）
	// 断点 2: tools 最后一项（工具列表会话内不变）
	// 断点 3+4: 最近 2 条非 system 消息（缓存会话历史前缀，多轮对话收益显著）
	if a.enablePromptCaching { //nolint:nestif
		cacheMarker := map[string]string{"type": "ephemeral"}

		// 断点 1 — system → text array + cache_control
		if system != "" {
			payload["system"] = []map[string]any{
				{"type": "text", "text": strings.TrimSpace(system), "cache_control": cacheMarker},
			}
		}
		// 断点 2 — tools 最后一项
		if tools, ok := payload["tools"].([]map[string]any); ok && len(tools) > 0 {
			tools[len(tools)-1]["cache_control"] = cacheMarker
		}
		// 断点 3+4 — 最近 2 条非 system 消息（按序收集非 system 下标，取末尾 2 条）
		var nonSysIdx []int
		for i, m := range msgs {
			if m["role"] != "system" {
				nonSysIdx = append(nonSysIdx, i)
			}
		}
		start := len(nonSysIdx) - 2
		if start < 0 {
			start = 0
		}
		for _, idx := range nonSysIdx[start:] {
			applyMsgCacheControl(msgs[idx], cacheMarker)
		}
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "buildAnthropicRequest: marshal payload", err)
	}
	return b, nil
}

// parseAnthropicStream 解析 Anthropic SSE 事件并转换为统一的 StreamEvent。
// tool_use 事件打包为 StreamToolCall，Content 为 JSON: {"id","name","input"}。
func (a *AnthropicAdapter) parseAnthropicStream(ctx context.Context, model string, body io.Reader, ch chan<- types.StreamEvent) { //nolint:gocyclo
	scanner := bufio.NewScanner(body)
	var toolID, toolName string
	var toolInputBuf strings.Builder
	inToolBlock := false
	inThinkingBlock := false // 标记当前是否在处理 extended thinking block

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

		switch frame.Type {
		case "message_start":
			if frame.Message.Usage.InputTokens > 0 {
				ch <- types.StreamEvent{
					Type: types.StreamTextDelta,
					Usage: types.Usage{
						InputTokens:         frame.Message.Usage.InputTokens,
						CacheHitTokens:      frame.Message.Usage.CacheReadInputTokens,
						CacheCreationTokens: frame.Message.Usage.CacheCreationInputTokens,
					},
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
				ch <- types.StreamEvent{Type: types.StreamThinking, Content: frame.Delta.Thinking}
			case !inToolBlock && !inThinkingBlock && frame.Delta.Type == "text_delta" && frame.Delta.Text != "":
				ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: frame.Delta.Text}
			}
		case "content_block_stop":
			if inToolBlock {
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
					ch <- types.StreamEvent{Type: types.StreamError, Content: fmt.Sprintf("tool_call payload marshal failed: %v", err)}
					inToolBlock = false
					inThinkingBlock = false
					continue
				}
				ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
				inToolBlock = false
			}
			inThinkingBlock = false
		case "message_delta":
			if frame.Usage.OutputTokens > 0 {
				ch <- types.StreamEvent{
					Type:  types.StreamTextDelta,
					Usage: types.Usage{OutputTokens: frame.Usage.OutputTokens},
				}
				if a.tbr != nil {
					a.tbr.Add(int64(frame.Usage.OutputTokens))
				}
			}
		case "message_stop":
			return
		}
	}
}

// applyMsgCacheControl 向单条消息的最后一个 content block 注入 cache_control。
// content 为 string 时转换为 text block 数组；为 []any 时直接在末尾元素追加。
func applyMsgCacheControl(msg map[string]any, marker map[string]string) {
	content := msg["content"]
	switch v := content.(type) {
	case string:
		msg["content"] = []map[string]any{
			{"type": "text", "text": v, "cache_control": marker},
		}
	case []any:
		if len(v) > 0 {
			if last, ok := v[len(v)-1].(map[string]any); ok {
				last["cache_control"] = marker
			}
		}
	}
}

func resolveAnthropicModel(requested string) string {
	switch requested {
	case "claude-instant-1.2", "claude-2.0", "claude-2.1":
		return "claude-3-5-haiku-latest"
	case "claude-3-opus-20240229":
		return "claude-3-5-sonnet-latest"
	default:
		if requested == "" {
			return "claude-3-5-sonnet-latest"
		}
		return requested
	}
}

// keyInjectRT injects the API key safely during the HTTP round trip.
//
// pool 取代原先的静态 keyFn func() []byte（2026-07-12 P1 修复）：每次 RoundTrip
// 从 CredentialPool 挑选一个可用凭证，注入后立即清零，并把本次调用的结果（含 HTTP
// 状态码）回报给该凭证——多 Key 场景下单个 Key 401/429/402 只会把它自己冷却，
// 不会拖垮整个 Provider（此前 credFn 是构造期固定的单 Key 闭包，无法感知失败/轮换）。
type keyInjectRT struct {
	inner http.RoundTripper
	pool  *llmparent.CredentialPool
}

func (rt keyInjectRT) RoundTrip(req *http.Request) (*http.Response, error) {
	cred := rt.pool.Pick()
	if cred == nil {
		return nil, apperr.New(apperr.CodeResourceExhausted, "keyInjectRT: no available credential (all keys cooling down)")
	}
	apiKey := cred.CredFn()()
	// 直接用 []byte 构造 canonical header value，避免 string() 产生不可清零副本。
	// http.Header 内部会 clone string，但此处我们在 RoundTrip 返回后立即
	// 删除 header 引用，将泄漏窗口收窄到单次 TCP write。
	req.Header.Set("x-api-key", unsafe.String(unsafe.SliceData(apiKey), len(apiKey)))
	resp, err := rt.inner.RoundTrip(req)
	req.Header.Del("x-api-key")  // 立即清除 header map 引用
	llmparent.ClearBytes(apiKey) // 清零原始 key 字节
	if err != nil {
		cred.RecordResult(err)
		return resp, apperr.Wrap(apperr.CodeInternal, "keyInjectRT.RoundTrip", err)
	}
	// 传输层成功但 HTTP 状态码非 200 时（401/402/429 等）也要回报，供 Classify()
	// 从合成的 "anthropic: HTTP {code}" 错误串中提取状态码并决定是否冷却本凭证；
	// 真正面向调用方的完整错误串仍由 Infer/StreamInfer 按响应体二次构造。
	if resp != nil && resp.StatusCode != 200 {
		cred.RecordResult(apperr.New(apperr.CodeInternal, fmt.Sprintf("anthropic: HTTP %d", resp.StatusCode)))
	} else {
		cred.RecordResult(nil)
	}
	return resp, nil
}
