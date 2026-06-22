package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
)

// World Model — 双层决策模型。
// L1: 调用前拦截 (StatePredictor + ConfidenceScorer)
// L2: [SurpriseIndex] 执行后调整
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §7

type WorldModel struct {
	provider protocol.Provider // P1: Knowledge Gap Awareness
}

// InjectProvider 注入 LLM 提供商
func (wm *WorldModel) InjectProvider(p protocol.Provider) {
	wm.provider = p
}

// AssessGrounding 评估上下文是否足够执行任务。
// 如果发现关键实体缺失，返回 false，提示需要进一步检索。
func (wm *WorldModel) AssessGrounding(ctx context.Context, task string, contextText string) (bool, string) {
	if wm.provider == nil || task == "" {
		return true, "" // 无 provider 或无任务则默认放行
	}

	prompt := fmt.Sprintf(
		"Task: %s\n\n"+
			"Current Context:\n%s\n\n"+
			"Assess if the current context provides sufficient information to execute the task.\n"+
			"If yes, reply with 'SUFFICIENT'.\n"+
			"If no, reply with 'INSUFFICIENT' and briefly explain what specific knowledge is missing.",
		task, contextText,
	)

	resp, err := wm.provider.Infer(ctx, []types.Message{{Role: "user", Content: prompt}}, types.WithMaxTokens(128))
	if err != nil {
		return true, "" // 评估失败默认放行
	}

	content := strings.TrimSpace(resp.Content)
	if strings.HasPrefix(strings.ToUpper(content), "INSUFFICIENT") {
		return false, content
	}
	return true, ""
}
