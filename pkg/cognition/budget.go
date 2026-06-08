package cognition

import (
	"fmt"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// 推理预算管理 — 四层预算体系。
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §8

// BudgetManager 四层推理预算。
type BudgetManager struct {
	maxReasoningSteps  int // 5
	maxThinkingTokens  int // 4096
	taskTokenBudget    int // 50K
	sessionTokenBudget int // 200K
	usedTokens         int
	Now                func() time.Time // 允许注入虚拟时间
}

// NewBudgetManager 创建带默认预算的管理器。
func NewBudgetManager() *BudgetManager {
	return &BudgetManager{
		maxReasoningSteps:  5,
		maxThinkingTokens:  4096,
		taskTokenBudget:    50000,
		sessionTokenBudget: 200000,
		usedTokens:         0,
		Now:                time.Now,
	}
}

// ConsumeTokens 消耗指定数量的 Tokens，若超出 Session 级预算则报错。
func (bm *BudgetManager) ConsumeTokens(tokens int) error {
	bm.usedTokens += tokens
	if bm.usedTokens > bm.sessionTokenBudget {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("session token budget exceeded: %d > %d", bm.usedTokens, bm.sessionTokenBudget))
	}
	return nil
}

// HasSufficientBudget 检查是否还有足够的 Session 预算。
func (bm *BudgetManager) HasSufficientBudget(requested int) bool {
	return bm.usedTokens+requested <= bm.sessionTokenBudget
}

// Limits 返回推理步数与思考 Token 限制。
func (bm *BudgetManager) Limits() (maxSteps, maxThinking int) {
	return bm.maxReasoningSteps, bm.maxThinkingTokens
}

// BudgetMode 推理预算模式。
type BudgetMode int

const (
	BudgetFixed    BudgetMode = iota // MaxReasoningSteps=5, MaxThinkingTokens=4096
	BudgetAdaptive                   // min(16384, 4096×(1+SurpriseIndex×3))
	BudgetBatch                      // 32K, 夜间
)

// SelectBudget 选择推理预算。
// IF inNightWindow(2-6am) AND NOT interactive → batch (32K)
// IF taskType IN (classification, summary, translation) → fixed (4K)
// ELSE → adaptive: min(16384, 4096 × (1 + surpriseIndex × 3))
// IF [TokenBurnRate] Stage1 THROTTLE → 降一档
func (bm *BudgetManager) SelectBudget(taskType string, surpriseIndex float64, isInteractive bool, burnStage int) BudgetMode {
	if bm.isNightWindow() && !isInteractive {
		return BudgetBatch
	}
	if isSimpleTask(taskType) {
		return BudgetFixed
	}
	if burnStage >= 1 {
		return BudgetFixed // THROTTLE → 降档
	}
	return BudgetAdaptive
}

// ContextWindowManager 上下文窗口管理器。
// maxTokens=90000. >70%→salience 排序压缩; >90%→语义结构感知逐出.
type ContextWindowManager struct {
	maxTokens    int // 90000
	currentUsage int
	softTrigger  float64 // 0.70
	hardTrigger  float64 // 0.90
}

// NeedsCompaction 判断是否需要压缩。
func (cwm *ContextWindowManager) NeedsCompaction() int {
	ratio := float64(cwm.currentUsage) / float64(cwm.maxTokens)
	if ratio > cwm.hardTrigger {
		return 2 // 硬触发 — 语义结构感知逐出
	}
	if ratio > cwm.softTrigger {
		return 1 // 软触发 — salience 排序压缩
	}
	return 0
}

func (bm *BudgetManager) isNightWindow() bool {
	hour := bm.Now().Hour()
	return hour >= 2 && hour < 6
}
func isSimpleTask(t string) bool {
	return t == "classification" || t == "summary" || t == "translation"
}
