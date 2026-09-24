package fsm

import (
	"strings"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

// RenderConversationHistory 把对话历史渲染为有界文本块（ADR-0098 决策四）。
//
// 自尾部截取：最近的上下文对理解本轮意图最有价值，被截掉的永远是最早的轮次。
// 纯函数（par_inv_03）：同输入同字节，供 Perceive/Respond 的 PromptFn 调用。
// maxMessages/maxBytes <= 0 表示该维度不限。
func RenderConversationHistory(history []types.Message, maxMessages, maxBytes int) string {
	blocks := make([]string, 0, len(history))
	for _, m := range history {
		if m.Role == "system" || strings.TrimSpace(m.Content) == "" {
			continue
		}
		blocks = append(blocks, "["+m.Role+"]\n"+m.Content)
	}
	if maxMessages > 0 && len(blocks) > maxMessages {
		blocks = blocks[len(blocks)-maxMessages:]
	}

	out := strings.Join(blocks, "\n\n")
	for maxBytes > 0 && len(out) > maxBytes && len(blocks) > 1 {
		blocks = blocks[1:]
		out = strings.Join(blocks, "\n\n")
	}
	if maxBytes > 0 && len(out) > maxBytes {
		out = tailRunes(out, maxBytes)
	}
	return out
}

// tailRunes 取 s 末尾不超过 maxBytes 字节的部分，且不切断多字节字符。
func tailRunes(s string, maxBytes int) string {
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
