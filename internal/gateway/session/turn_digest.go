package session

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

// 回合摘要（ADR-0120 决策四第 2 条）：内核每回合新建 Agent，会话历史只带 user/assistant 文本，
// 下一回合不知道上一回合调用过哪些工具、读过哪些文件，只能重读。这里把每个历史回合的工具步骤
// 机械压成几行附在该回合的 assistant 消息后（不调 LLM）。步骤在回合结束后不再变化，摘要字节稳定，
// 不破坏历史层的前缀缓存。只进入发给内核的历史，不改库、不改前端展示。

const (
	turnDigestMaxChars = 600 // 单回合摘要总长上限（字符）
	turnDigestArgChars = 60  // 每行参数预览上限
)

// turnDigestSource 可选能力：TurnStore 实现它时启用回合摘要（接口在消费方定义，HE-3）。
type turnDigestSource interface {
	SessionToolSteps(ctx context.Context, sessionID string) (map[string][]Step, error)
}

// withTurnDigests 返回附加了回合摘要的历史副本；不可用或查询失败时原样返回。
func (o *orchestrator) withTurnDigests(ctx context.Context, sessionID string, history []types.Message) []types.Message {
	src, ok := o.turns.(turnDigestSource)
	if !ok || src == nil {
		return history
	}
	steps, err := src.SessionToolSteps(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "session: load tool steps for turn digest failed", "session", sessionID, "err", err)
		return history
	}
	if len(steps) == 0 {
		return history
	}
	out := make([]types.Message, len(history))
	copy(out, history)
	for i, m := range out {
		if m.Role != "assistant" || m.TurnID == "" {
			continue
		}
		if d := buildTurnDigest(steps[m.TurnID]); d != "" {
			out[i].Content = m.Content + "\n\n" + d
		}
	}
	return out
}

// buildTurnDigest 每个工具步骤一行：`工具(参数预览) → 状态 [结果规模]`；总长超限时截断并注明省略条数。
func buildTurnDigest(steps []Step) string {
	if len(steps) == 0 {
		return ""
	}
	var lines []string
	used := 0
	for i, st := range steps {
		name := st.Tool
		if name == "" {
			name = st.Title
		}
		line := fmt.Sprintf("%s(%s) → %s", name, clipRunes(oneLine(st.ArgsPreview), turnDigestArgChars), st.Status)
		if n := utf8.RuneCountInString(st.ResultPreview); n > 0 {
			line += fmt.Sprintf(" [%d 字]", n)
		}
		if used+utf8.RuneCountInString(line) > turnDigestMaxChars {
			lines = append(lines, fmt.Sprintf("…（另有 %d 次工具调用未列出）", len(steps)-i))
			break
		}
		used += utf8.RuneCountInString(line)
		lines = append(lines, line)
	}
	return "<turn_digest>\n" + strings.Join(lines, "\n") + "\n</turn_digest>"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
