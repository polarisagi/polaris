package adapter

import "github.com/polarisagi/polaris/pkg/types"

// ============================================================================
// Anthropic 非首部 system 内联：消息条目模型 + 合并（见 inline_system.go 顶部说明）。
// 仅在 AnthropicAdapter.inlineNonLeadingSystem 开启时使用；关闭时 buildAnthropicRequest
// 走旧路径，请求体字节不变。
// ============================================================================

// anthropicBlock 是一个 content block 及其"应带断点"标记（源自 Message.CacheBreakpoint）。
type anthropicBlock struct {
	v    any
	flag bool
}

// anthropicEntry 是一条待发送的 Anthropic 消息。plain 条目（无 Parts、未合并）序列化为字符串
// content，与旧路径字节一致；合并或带 Parts 后为 block 数组。
type anthropicEntry struct {
	role    string
	plain   bool
	text    string
	flagged bool // plain 条目的消息级断点
	blocks  []anthropicBlock
	inline  bool // 含内联 system 块：允许与相邻 user 条目合并
}

func anthropicTextBlock(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

// toBlocks 把 plain 条目展开为 block 形式（空文本不产生块，Anthropic 拒绝空 text block）。
func (e *anthropicEntry) toBlocks() {
	if !e.plain {
		return
	}
	e.plain = false
	if e.text != "" {
		e.blocks = []anthropicBlock{{v: anthropicTextBlock(e.text), flag: e.flagged}}
	}
}

func isToolResultBlock(b anthropicBlock) bool {
	m, ok := b.v.(map[string]any)
	return ok && m["type"] == "tool_result"
}

// escapeUserBlocks 对 user 角色 Parts 中的文本与工具结果字符串转义 <system_instruction> 标签字面。
// 命中才复制 map，不修改调用方持有的 Parts。
func escapeUserBlocks(blocks []any) []any {
	for i, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		key := ""
		switch m["type"] {
		case "text":
			key = "text"
		case "tool_result":
			key = "content"
		default:
			continue
		}
		s, ok := m[key].(string)
		if !ok {
			continue
		}
		if es := escapeSystemInstructionTag(s); es != s {
			cp := make(map[string]any, len(m))
			for k, v := range m {
				cp[k] = v
			}
			cp[key] = es
			blocks[i] = cp
		}
	}
	return blocks
}

// appendAnthropicEntry 把一条消息追加为条目：非首部 system → user 角色 <system_instruction> 块，
// 并与紧邻的 user 条目按 Anthropic 角色交替规则合并（保持原相对顺序；工具结果块前置）。
func appendAnthropicEntry(ents []anthropicEntry, m types.Message) []anthropicEntry {
	e := anthropicEntry{role: m.Role}
	switch {
	case m.Role == "system":
		body := wrapInlineSystem(m.Content)
		if body == "" {
			return ents
		}
		e.role, e.inline = "user", true
		e.blocks = []anthropicBlock{{v: anthropicTextBlock(body), flag: m.CacheBreakpoint}}
	case len(m.Parts) > 0:
		vs := anthropicPartBlocks(m.Parts)
		if m.Role == "user" {
			vs = escapeUserBlocks(vs)
		}
		e.blocks = make([]anthropicBlock, len(vs))
		for i, v := range vs {
			e.blocks[i] = anthropicBlock{v: v}
		}
		e.blocks[len(e.blocks)-1].flag = m.CacheBreakpoint
	default:
		e.plain, e.text, e.flagged = true, m.Content, m.CacheBreakpoint
		if m.Role == "user" {
			e.text = escapeSystemInstructionTag(e.text)
		}
	}
	if e.role == "user" && len(ents) > 0 {
		prev := &ents[len(ents)-1]
		if prev.role == "user" && (prev.inline || e.inline) {
			prev.toBlocks()
			e.toBlocks()
			prev.blocks = stablePartition(append(prev.blocks, e.blocks...), isToolResultBlock)
			prev.inline = true
			return ents
		}
	}
	return append(ents, e)
}

// renderAnthropicEntries 把条目序列化为 messages，并给出消息级/块级断点定位。
func renderAnthropicEntries(ents []anthropicEntry) (msgs []map[string]any, flagMsg map[int]bool, flagBlk map[int][]int) {
	flagMsg, flagBlk = map[int]bool{}, map[int][]int{}
	for i, e := range ents {
		if e.plain {
			msgs = append(msgs, map[string]any{"role": e.role, "content": e.text})
			if e.flagged {
				flagMsg[i] = true
			}
			continue
		}
		vs := make([]any, len(e.blocks))
		for j, b := range e.blocks {
			vs[j] = b.v
			if b.flag {
				flagBlk[i] = append(flagBlk[i], j)
				flagMsg[i] = true
			}
		}
		msgs = append(msgs, map[string]any{"role": e.role, "content": vs})
	}
	return msgs, flagMsg, flagBlk
}
