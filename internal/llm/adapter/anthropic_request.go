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
	// 调用方在缓存层末尾置位的 Message.CacheBreakpoint（ADR-0105 决策一）：
	// 记录其在 systemParts / msgs 中的下标；无任何置位时走下方内置启发式。
	flagSys := map[int]bool{}
	flagMsg := map[int]bool{}
	// flagBlk 仅内联模式使用：被置位消息中断点应落的 content block 下标（合并后块级定位）。
	flagBlk := map[int][]int{}
	var ents []anthropicEntry
	leading := true
	for _, m := range req.Messages {
		// 关闭内联：所有 system 都进 system 参数（旧行为）；开启：只有开头连续的 system 进入。
		if m.Role == "system" && (leading || !a.inlineNonLeadingSystem) {
			system += m.Content + "\n"
			if t := strings.TrimSpace(m.Content); t != "" {
				systemParts = append(systemParts, t)
				if m.CacheBreakpoint {
					flagSys[len(systemParts)-1] = true
				}
			}
			continue
		}
		leading = false
		if a.inlineNonLeadingSystem {
			ents = appendAnthropicEntry(ents, m)
			continue
		}
		if m.CacheBreakpoint {
			flagMsg[len(msgs)] = true
		}
		if len(m.Parts) > 0 {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": anthropicPartBlocks(m.Parts)})
		} else {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	if a.inlineNonLeadingSystem {
		msgs, flagMsg, flagBlk = renderAnthropicEntries(ents)
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
		if tc := anthropicToolChoice(req.ToolChoice); tc != nil {
			payload["tool_choice"] = tc
		}
	}

	// Anthropic Prompt Caching（ADR-0102 决策三 / ADR-0105 决策三），最多 4 个断点。缓存前缀顺序为
	// tools → system → messages，断点缓存其之前的全部内容。
	// 断点 1（L0 末）: 第一个 system block——ImmutableCore（人格/工具摘要/偏好，跨会话、跨阶段
	//         稳定）连同其前的 tools 一并缓存；无 system 时退回 tools 末尾。
	// 有 Message.CacheBreakpoint 标记时（prompt 组装方在 L2 历史末置位）：
	//   断点 2: 标记消息（最多取最靠后的若干个，受 4 上限约束）；
	//   断点 3: 最后一条非 system 消息（本轮增量）。
	// 无标记时回退旧启发式：末个 system block + 最近 2 条非 system 消息。
	if a.enablePromptCaching {
		a.applyPromptCaching(payload, msgs, systemParts, flagSys, flagMsg, flagBlk)
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

// anthropicCacheMarker 构造 cache_control 标记。ttl 为 "1h" 时显式下发；5m 是 API 默认，不下发以保持
// 请求体与旧版字节一致。同一请求内所有断点使用同一 TTL（API 要求长 TTL 条目排在短 TTL 之前）。
func (a *AnthropicAdapter) anthropicCacheMarker() map[string]string {
	m := map[string]string{"type": "ephemeral"}
	if a.cacheTTL == "1h" {
		m["ttl"] = "1h"
	}
	return m
}

// maxAnthropicBreakpoints Anthropic 显式缓存断点上限。
const maxAnthropicBreakpoints = 4

// applyPromptCaching 按 ADR-0105 决策一的层对齐向 payload 注入 cache_control 断点，总数不超过 4。
func (a *AnthropicAdapter) applyPromptCaching(payload map[string]any, msgs []map[string]any, systemParts []string, flagSys, flagMsg map[int]bool, flagBlk map[int][]int) {
	marker := a.anthropicCacheMarker()
	used := 0
	if len(systemParts) > 0 {
		used++ // 断点 1：首个 system block（L0 末），在下方渲染时落点
	} else if tools, ok := payload["tools"].([]map[string]any); ok && len(tools) > 0 {
		// 无 system 时退回在 tools 末尾打点，保住工具定义前缀。
		tools[len(tools)-1]["cache_control"] = marker
		used++
	}

	budget := maxAnthropicBreakpoints - used
	if lastMsgNeedsTwoBreakpoints(msgs, flagMsg, flagBlk) {
		budget-- // 末条消息既是"最后一条"又带一个非末尾的层断点块：占两个名额
	}
	markSys, markMsg := pickBreakpoints(len(msgs), len(systemParts), flagSys, flagMsg, budget)
	if len(systemParts) > 0 {
		markSys[0] = true
		blocks := make([]map[string]any, len(systemParts))
		for i, part := range systemParts {
			blocks[i] = map[string]any{"type": "text", "text": part}
			if markSys[i] {
				blocks[i]["cache_control"] = marker
			}
		}
		payload["system"] = blocks
	}
	for i := range msgs {
		if !markMsg[i] {
			continue
		}
		blks := flagBlk[i]
		// 无块级定位（旧路径/未合并）或该条是"最后一条"或走旧启发式时，打在末块；
		// 被置位且有块级定位的消息，断点落在其对应内容块上。
		lastPick := len(blks) == 0 || i == len(msgs)-1 || len(flagMsg) == 0
		markMsgBlocks(msgs[i], marker, blks, lastPick)
	}
}

// lastMsgNeedsTwoBreakpoints 判断末条消息是否带有落在非末块上的层断点（内联合并后 L2 末的 user 块
// 与 L3/L4 内联块同处一条 user 消息时出现）。
func lastMsgNeedsTwoBreakpoints(msgs []map[string]any, flagMsg map[int]bool, flagBlk map[int][]int) bool {
	n := len(msgs)
	if n == 0 || !flagMsg[n-1] {
		return false
	}
	blks := flagBlk[n-1]
	content, ok := msgs[n-1]["content"].([]any)
	return ok && len(blks) > 0 && blks[len(blks)-1] != len(content)-1
}

// markMsgBlocks 在 blks 指定的 content block 上写 cache_control；lastPick 时另在末块写。
// 写入前复制被标记的 block map，避免污染调用方持有的 Parts。
func markMsgBlocks(msg map[string]any, marker map[string]string, blks []int, lastPick bool) {
	if content, ok := msg["content"].([]any); ok && len(blks) > 0 {
		nc := append([]any(nil), content...)
		for _, j := range blks {
			if j < 0 || j >= len(nc) {
				continue
			}
			if bm, ok := nc[j].(map[string]any); ok {
				cp := make(map[string]any, len(bm)+1)
				for k, v := range bm {
					cp[k] = v
				}
				cp["cache_control"] = marker
				nc[j] = cp
			}
		}
		msg["content"] = nc
	}
	if lastPick {
		applyMsgCacheControl(msg, marker)
	}
}

// pickBreakpoints 在剩余名额 budget 内选出需要断点的 system block 与非 system 消息下标
// （不含首个 system block，其断点由调用方固定放置）。
//
// 无任何 CacheBreakpoint 标记：回退旧启发式——末个 system block + 最近 2 条非 system 消息。
// 有标记：最后一条消息优先占名额，其后标记消息按位置从后向前占用剩余名额
// （历史越靠后越可能被下一轮复用），最后才是标记的 system block。
func pickBreakpoints(nMsgs, nSys int, flagSys, flagMsg map[int]bool, budget int) (markSys, markMsg map[int]bool) {
	markSys, markMsg = map[int]bool{}, map[int]bool{}
	if len(flagSys) == 0 && len(flagMsg) == 0 {
		if nSys > 1 {
			markSys[nSys-1] = true
		}
		for i := nMsgs - 1; i >= 0 && i >= nMsgs-2; i-- {
			markMsg[i] = true
		}
		return markSys, markMsg
	}
	if nMsgs > 0 && budget > 0 {
		markMsg[nMsgs-1] = true
		budget--
	}
	for i := nMsgs - 1; i >= 0 && budget > 0; i-- {
		if flagMsg[i] && !markMsg[i] {
			markMsg[i] = true
			budget--
		}
	}
	for i := nSys - 1; i > 0 && budget > 0; i-- {
		if flagSys[i] {
			markSys[i] = true
			budget--
		}
	}
	return markSys, markMsg
}

// anthropicToolChoice 把内部 ToolChoice 映射为 Anthropic tool_choice；未知值返回 nil（不下发）。
// tool_choice 变化只使 messages 缓存失效，不影响工具定义与 system 前缀。
func anthropicToolChoice(choice string) map[string]any {
	switch choice {
	case "none", "auto":
		return map[string]any{"type": choice}
	case "required", "any":
		return map[string]any{"type": "any"}
	}
	return nil
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

// anthropicPartBlocks 把 Message.Parts 转成 Anthropic content blocks：ImagePart → base64 image，
// 其余（text/tool_use/tool_result 的 map）原样透传。
func anthropicPartBlocks(parts []any) []any {
	var contentBlocks []any
	for _, p := range parts {
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
	return contentBlocks
}
