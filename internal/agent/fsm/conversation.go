package fsm

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

// 对话历史窗口（ADR-0105 决策二）：只追加 + 分块跳窗。
//
// 逐条滑动的窗口每回合都改变首条消息，整个 L2 前缀永不命中缓存；分块跳窗让 L2 在两次
// 跳窗之间纯追加，跳窗一次丢掉窗口的前半（约 maxMessages/2 条）。跳窗切点是"历史条数与
// 字节数"的纯函数，不依赖任何跨调用状态，因此：
//   - 同一回合四个阶段（同一份历史）得到字节相同的 L2；
//   - 相邻两回合（历史只追加了几条）在未越过跳窗阈值时，前者的 L2 是后者的前缀。

const (
	// historyAnchorMaxItems 锚定摘要至多列出被丢弃消息中最近的 N 条（有界，防止长会话摘要无限增长）。
	historyAnchorMaxItems = 8
	// historyAnchorLineRunes 摘要中每条消息首行的截断长度（rune）。
	historyAnchorLineRunes = 80
	// historyDefaultStep 仅在 maxMessages 不限时（<=0），按字节跳窗所用的步长（条）。
	historyDefaultStep = 10
)

// WindowConversationHistory 把对话历史整理为 L2 消息序列：过滤 system 与空消息，按真实
// 角色（assistant / 其余一律 user——历史里不会有可回放的 tool 消息，裸 tool 角色会被
// Provider 以缺 tool_call_id 拒绝）逐条输出。越过 maxMessages/maxBytes（<=0 表示该维度
// 不限）时一次丢掉前面若干块，并在首位放一条确定性锚定摘要（不调用 LLM）。
//
// 只返回 Role/Content：历史不携带 ReasoningContent——DeepSeek 要求原样回传的是同一回合内
// tool_call 往返的思考内容，而那部分不经会话历史，跨回合的思考链回传既无必要又会污染前缀。
//
// 纯函数（par_inv_03）：同输入同字节。
func WindowConversationHistory(history []types.Message, maxMessages, maxBytes int) (anchor string, kept []types.Message) {
	msgs := make([]types.Message, 0, len(history))
	for _, m := range history {
		if m.Role == "system" || strings.TrimSpace(m.Content) == "" {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		content := m.Content
		// 单条上限：超大单条只保留尾部。截断只取决于这条消息自身，不破坏"旧消息字节不变"。
		if maxBytes > 0 && len(content) > maxBytes {
			content = tailRunes(content, maxBytes)
		}
		msgs = append(msgs, types.Message{Role: role, Content: content})
	}

	drop := historyDropCount(msgs, maxMessages, maxBytes)
	if drop == 0 {
		return "", msgs
	}
	return renderHistoryAnchor(msgs[:drop]), msgs[drop:]
}

// historyDropCount 计算需要丢弃的前缀条数。它是历史内容的单调阶梯函数：历史只追加时
// 返回值只增不减，且只在越过阈值时以整块（step 条）跳变。
func historyDropCount(msgs []types.Message, maxMessages, maxBytes int) int {
	n := len(msgs)
	if n <= 1 {
		return 0
	}
	step := historyDefaultStep
	if maxMessages > 0 {
		step = maxMessages / 2
		if step < 1 {
			step = 1
		}
	}

	drop := 0
	if maxMessages > 0 && n > maxMessages {
		over := n - maxMessages
		drop = step * ((over + step - 1) / step)
	}
	if drop > n-1 {
		drop = n - 1
	}

	if maxBytes > 0 {
		total := 0
		for _, m := range msgs[drop:] {
			total += len(m.Content)
		}
		// 字节超限继续按块前移；至少保留最后一条（本轮最相关的上文）。
		for total > maxBytes && drop < n-1 {
			next := drop + step
			if next > n-1 {
				next = n - 1
			}
			for _, m := range msgs[drop:next] {
				total -= len(m.Content)
			}
			drop = next
		}
	}
	return drop
}

// renderHistoryAnchor 生成被丢弃部分的确定性概要：总条数 + 最近若干条的角色与首行截断。
// 内容取自不可信历史，调用方必须按 TaintHigh 围栏写入（它是数据不是指令）。
func renderHistoryAnchor(dropped []types.Message) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "较早的 %d 条对话已省略（以下仅为确定性概要，非完整内容）：", len(dropped))
	from := 0
	if len(dropped) > historyAnchorMaxItems {
		from = len(dropped) - historyAnchorMaxItems
		fmt.Fprintf(&sb, "\n- …（更早的 %d 条未列出）", from)
	}
	for _, m := range dropped[from:] {
		fmt.Fprintf(&sb, "\n- %s: %s", m.Role, firstLineRunes(m.Content, historyAnchorLineRunes))
	}
	return sb.String()
}

// firstLineRunes 取首个非空行并按 rune 截断。
func firstLineRunes(s string, maxRunes int) string {
	line := strings.TrimSpace(s)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if utf8.RuneCountInString(line) <= maxRunes {
		return line
	}
	r := []rune(line)
	return string(r[:maxRunes]) + "…"
}

// tailRunes 取 s 末尾不超过 maxBytes 字节的部分，且不切断多字节字符。
func tailRunes(s string, maxBytes int) string {
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
