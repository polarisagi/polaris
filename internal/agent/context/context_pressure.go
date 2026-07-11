package agentctx

import (
	"fmt"

	"github.com/polarisagi/polaris/internal/agent/fsm"
)

// contextPressureHighRatio / contextPressureMediumRatio 三档上下文压力提示阈值
// （TokensUsed/TokenBudget 比例）。未采用 Go 侧强制触发（那是 ForgettingManager
// 的职责），仅作为提示强度分级。
const (
	contextPressureHighRatio   = 0.85
	contextPressureMediumRatio = 0.65
)

// contextPressureHint 生成"当前上下文压力"提示文本，注入 System Prompt 供 LLM
// 自主判断是否调用 memory_page_out 释放 Core Memory 空间（GD-14-002 最小实现，
// 见 internal/tool/builtin/memory_tools.go memoryPageOutTool 设计说明）。
//
// 信号来源：sCtx.TokensUsed / sCtx.TokenBudget（Agent 会话级预算，Budget
// Manager 已维护的既有字段，非新增埋点）。TokenBudget<=0 表示预算未配置，
// 不产生提示（不能除零，也没有意义的比例可算）。
//
// 设计原则（任务书 8 §8.5 步骤 3 明确要求）：是否/何时调用 memory_page_out
// 完全由 LLM 自主决定，本函数只负责"暴露信号"，不做强制阈值触发判断之外的
// 任何副作用（不修改 sCtx、不调用任何工具）。
func contextPressureHint(sCtx *fsm.StateContext) string {
	if sCtx == nil || sCtx.TokenBudget <= 0 {
		return ""
	}
	ratio := float64(sCtx.TokensUsed) / float64(sCtx.TokenBudget)
	switch {
	case ratio >= contextPressureHighRatio:
		return fmt.Sprintf(
			"Context Pressure: HIGH (%.0f%% of token budget used). Core memory may be crowded with "+
				"stale content. Consider calling memory_page_out on blocks you no longer need visible "+
				"every turn (they remain retrievable via memory_search / memory_page_in).",
			ratio*100)
	case ratio >= contextPressureMediumRatio:
		return fmt.Sprintf(
			"Context Pressure: moderate (%.0f%% of token budget used). Keep an eye on core memory size.",
			ratio*100)
	default:
		return ""
	}
}
