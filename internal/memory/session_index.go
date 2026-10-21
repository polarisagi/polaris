package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

const (
	sessionIndexMaxRunes    = 1200
	sessionIndexEntityCount = 12
	sessionIndexNameRunes   = 40
)

// entityDigester 由语义层实现；未实现（Tier-0 / 测试替身）时索引不含实体行。
type entityDigester interface {
	EntityDigest(ctx context.Context, limit int) (int, []string, error)
}

// SessionIndex 渲染会话级记忆索引（ADR-0120 决策六）：核心记忆块键与字数、长期记忆条数与最近实体名。
// 会话层首次组装时调用一次并随会话层冻结；让模型据此决定何时 memory_search / session_search，
// 而不是被动注入召回。无任何内容时返回空串。输出 ≤ sessionIndexMaxRunes 个字符。
func (f *MemoryFacadeImpl) SessionIndex(ctx context.Context, agentID, sessionID string) (string, error) {
	blocks, err := f.ListCoreMemory(ctx, agentID, sessionID)
	if err != nil {
		blocks = nil // 核心记忆读失败按无处理，不阻断索引
	}
	var total int
	var names []string
	if d, ok := f.sys.Mem().Semantic().(entityDigester); ok {
		if n, ns, dErr := d.EntityDigest(ctx, sessionIndexEntityCount); dErr == nil {
			total, names = n, ns
		}
	}
	return renderSessionIndex(blocks, total, names), nil
}

func renderSessionIndex(blocks []types.CoreMemoryBlock, entityTotal int, names []string) string {
	var parts []string
	if len(blocks) > 0 {
		items := make([]string, 0, len(blocks))
		for _, b := range blocks {
			items = append(items, fmt.Sprintf("%s(%d)", b.BlockKey, utf8.RuneCountInString(b.Content)))
		}
		parts = append(parts, "core memory blocks (key(chars)): "+strings.Join(items, ", "))
	}
	if entityTotal > 0 {
		line := fmt.Sprintf("long-term memory: %d entities", entityTotal)
		if len(names) > 0 {
			clipped := make([]string, 0, len(names))
			for _, n := range names {
				clipped = append(clipped, clipRunes(strings.Join(strings.Fields(n), " "), sessionIndexNameRunes))
			}
			line += "; recent: " + strings.Join(clipped, ", ")
		}
		parts = append(parts, line)
	}
	if len(parts) == 0 {
		return ""
	}
	parts = append(parts, "search it with memory_search; past conversations with session_search.")
	return clipRunes(strings.Join(parts, "\n"), sessionIndexMaxRunes)
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
