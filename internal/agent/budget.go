package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 推理预算管理 — 四层预算体系。
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §8

// 编译期断言：BudgetManager 必须满足 fsm.BudgetController 接口。
var _ fsm.BudgetController = (*BudgetManager)(nil)

// BudgetManager 四层推理预算。
type BudgetManager struct {
	mu                 sync.RWMutex
	maxReasoningSteps  int // 5
	maxThinkingTokens  int // 4096
	taskTokenBudget    int // 1M
	sessionTokenBudget int // 5M
	usedTokens         int
	Now                func() time.Time // 允许注入虚拟时间
}

// NewBudgetManager 创建带默认预算的管理器。
func NewBudgetManager() *BudgetManager {
	return &BudgetManager{
		maxReasoningSteps:  5,
		maxThinkingTokens:  4096,
		taskTokenBudget:    1000000,
		sessionTokenBudget: 5000000,
		usedTokens:         0,
		Now:                time.Now,
	}
}

// ConsumeTokens 消耗指定数量的 Tokens，若超出 Session 级预算则报错。
func (bm *BudgetManager) ConsumeTokens(tokens int) error {
	bm.mu.Lock()
	bm.usedTokens += tokens
	used := bm.usedTokens
	budget := bm.sessionTokenBudget
	bm.mu.Unlock()
	// HE-1: Token_Burn_Rate 一等公民上报
	trace.RecordBudgetTokens(context.Background(), tokens)
	if used > budget {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("session token budget exceeded: %d > %d", used, budget))
	}
	return nil
}

// HasSufficientBudget 检查是否还有足够的 Session 预算（不扣除）。
func (bm *BudgetManager) HasSufficientBudget(requested int) bool {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	return bm.usedTokens+requested <= bm.sessionTokenBudget
}

// EstimatedSpendUSD 基于会话内已消耗 token 数的近似估算，非真实持久化月度账本。
// 真实月度聚合需要新表（033_billing_ledger.sql），超出本任务范围，留待后续。
const estimatedUSDPerMillionTokens = 3.0 // 粗略估算系数，非精确计费

func (bm *BudgetManager) EstimatedSpendUSD() float64 {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	return float64(bm.usedTokens) / 1_000_000.0 * estimatedUSDPerMillionTokens
}

// UsedTokens 返回已消耗的 token 数（用于 sCtx.TokensUsed 同步）。
func (bm *BudgetManager) UsedTokens() int {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	return bm.usedTokens
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
