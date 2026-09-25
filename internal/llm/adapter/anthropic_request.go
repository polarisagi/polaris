package adapter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
	// systemParts 保留各 system 消息边界：缓存断点要落在"稳定前缀"之后（见下方
	// Prompt Caching 段），整段拼接后只能在末尾打一个断点。
	var systemParts []string
	for _, m := range req.Messages {
		if m.Role == "system" {
			system += m.Content + "\n"
			if t := strings.TrimSpace(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
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

	// Anthropic Prompt Caching（ADR-0101 决策三），最多 4 个断点。缓存前缀顺序为
	// tools → system → messages，断点缓存其之前的全部内容：
	// 断点 1: 第一个 system block——ImmutableCore（人格/工具摘要/偏好，跨会话、跨阶段
	//         稳定）连同其前的 tools 一并缓存。此前 system 整段拼接只在末尾打点，
	//         阶段指令/核心记忆/工具目录任一变化即令人格前缀整段失配。
	// 断点 2: 最后一个 system block——同阶段同会话内稳定的完整 system。
	// 断点 3+4: 最近 2 条非 system 消息（会话历史前缀）。
	if a.enablePromptCaching { //nolint:nestif
		cacheMarker := map[string]string{"type": "ephemeral"}

		if len(systemParts) > 0 {
			blocks := make([]map[string]any, len(systemParts))
			for i, part := range systemParts {
				blocks[i] = map[string]any{"type": "text", "text": part}
			}
			blocks[0]["cache_control"] = cacheMarker
			blocks[len(blocks)-1]["cache_control"] = cacheMarker
			payload["system"] = blocks
		} else if tools, ok := payload["tools"].([]map[string]any); ok && len(tools) > 0 {
			// 无 system 时退回在 tools 末尾打点，保住工具定义前缀。
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
	// 改为使用 string() 普通拷贝，牺牲一次小内存分配换取语义正确性，避免 defer/清理 时修改已赋值给 http.Header 的字符串底层内存导致内存竞态。
	// http.Header 内部会 clone string，但此处我们在 RoundTrip 返回后立即
	// 删除 header 引用，将泄漏窗口收窄到单次 TCP write。
	req.Header.Set("x-api-key", string(apiKey))
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
