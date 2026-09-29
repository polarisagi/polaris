package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 推理预算管理 — 四层预算体系。
// 架构文档: docs/arch/M04-Agent-Kernel.md §8

// 编译期断言：BudgetManager 必须满足 fsm.BudgetController 接口。
var _ fsm.BudgetController = (*BudgetManager)(nil)

// LLMSpendLedger llm_calls 账本的只读视图（消费端接口，实现见 internal/store/repo）。
// 账本由 internal/llm 的记录包装异步逐次写入，读到的用量可能滞后最近一次调用——
// 预算是软熔断而非计费，ADR-0104 决策四已接受该滞后。
type LLMSpendLedger interface {
	// SessionTokens 该会话累计 input+output token。
	SessionTokens(ctx context.Context, sessionID string) (int64, error)
	// SpendUSDSince 自 sinceMs（Unix 毫秒）起全部会话的累计 cost_usd。
	SpendUSDSince(ctx context.Context, sinceMs int64) (float64, error)
}

// MonthlyLimitSource 月度预算上限来源（repo.BudgetRepository 的只读子集）。
type MonthlyLimitSource interface {
	GetBudget(ctx context.Context) (float64, error)
}

// monthlyLimitTTL 月度上限缓存时长：HandleSetBudget 写库后最迟此时长内对所有 Agent 生效，
// 同时避免每次 DAG 校验都打库。
const monthlyLimitTTL = 30 * time.Second

// BudgetManager 四层推理预算。用量与花费不再持有内存计数，一律以 llm_calls 为唯一账本
// （ADR-0104 决策四）：重启不清零，池化会话 Agent 与 agent-0 口径一致。
type BudgetManager struct {
	maxReasoningSteps  int              // 5
	maxThinkingTokens  int              // 4096
	taskTokenBudget    int              // 1M
	sessionTokenBudget int              // 5M
	Now                func() time.Time // 允许注入虚拟时间

	sessionID string
	ledger    LLMSpendLedger
	limits    MonthlyLimitSource

	limitMu       sync.Mutex
	cachedLimit   float64
	limitLoadedAt time.Time
	limitLoaded   bool
}

// NewBudgetManager 创建带默认预算的管理器。未绑定账本时（WithLedger 前）不做用量判定。
func NewBudgetManager() *BudgetManager {
	return &BudgetManager{
		maxReasoningSteps:  5,
		maxThinkingTokens:  4096,
		taskTokenBudget:    1000000,
		sessionTokenBudget: 5000000,
		Now:                time.Now,
	}
}

// WithLedger 绑定所属会话、账本与月度上限来源。sessionID 为空时不做会话级判定
// （按空 session_id 求和会把全部后台调用算进来）。
func (bm *BudgetManager) WithLedger(sessionID string, ledger LLMSpendLedger, limits MonthlyLimitSource) *BudgetManager {
	bm.sessionID = sessionID
	bm.ledger = ledger
	bm.limits = limits
	return bm
}

// ConsumeTokens 上报本次消耗（仅埋点），并以账本判定会话累计用量是否超出 Session 级预算。
// 账本读失败时 fail-open（Warn 后放行）：预算是软熔断，不能因 DB 抖动让所有对话失败；
// 硬性安全边界（Cedar/Taint/KillSwitch）不依赖本检查，ADR-0087 的 fail-closed 清单不含预算。
func (bm *BudgetManager) ConsumeTokens(ctx context.Context, tokens int) error {
	// HE-1: Token_Burn_Rate 一等公民上报
	trace.RecordBudgetTokens(ctx, tokens)
	if bm.ledger == nil || bm.sessionID == "" {
		return nil
	}
	used, err := bm.ledger.SessionTokens(ctx, bm.sessionID)
	if err != nil {
		slog.Warn("budget: 读取会话 token 账本失败，放行", "session", bm.sessionID, "err", err)
		return nil
	}
	if used > int64(bm.sessionTokenBudget) {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("session token budget exceeded: %d > %d", used, bm.sessionTokenBudget))
	}
	return nil
}

// MonthlySpendUSD 当月（UTC）累计真实花费；读失败返回 0（放行，理由同 ConsumeTokens）。
func (bm *BudgetManager) MonthlySpendUSD(ctx context.Context) float64 {
	if bm.ledger == nil {
		return 0
	}
	now := bm.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	spend, err := bm.ledger.SpendUSDSince(ctx, monthStart.UnixMilli())
	if err != nil {
		slog.Warn("budget: 读取月度花费账本失败，按 0 处理", "err", err)
		return 0
	}
	return spend
}

// MonthlyLimitUSD 月度上限（0 = 不限额），带 monthlyLimitTTL 缓存；读失败沿用旧值，
// 从未成功读过则按不限额处理（fail-open，理由同 ConsumeTokens）。
func (bm *BudgetManager) MonthlyLimitUSD(ctx context.Context) float64 {
	if bm.limits == nil {
		return 0
	}
	bm.limitMu.Lock()
	defer bm.limitMu.Unlock()
	now := bm.Now()
	if bm.limitLoaded && now.Sub(bm.limitLoadedAt) < monthlyLimitTTL {
		return bm.cachedLimit
	}
	v, err := bm.limits.GetBudget(ctx)
	if err != nil {
		slog.Warn("budget: 读取月度预算上限失败，沿用缓存值", "err", err)
		return bm.cachedLimit
	}
	bm.cachedLimit, bm.limitLoadedAt, bm.limitLoaded = v, now, true
	return v
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

// defaultContextWindowMaxTokens M04 §7 规定的 M4 热路径上下文窗口容量。
const defaultContextWindowMaxTokens = 90000

const (
	contextWindowSoftTrigger = 0.70
	contextWindowHardTrigger = 0.90
)

// ContextWindowManager 上下文窗口管理器（M04-Agent-Kernel.md §7 热路径压缩）。
// maxTokens=90000. >70%→salience 排序压缩; >90%→语义结构感知逐出.
//
// 2026-07-22 一致性审查修复（ADR-0062 DEFER 项闭环，见 ADR-0033）：此前本
// 类型从未被构造、currentUsage 从未被赋值，NeedsCompaction 恒定基于零值
// 计算——M4 主循环的 50/75/100% 三级检测（agent_execute_effect.go）只做
// [BUDGET_CONSTRAINT] 提示注入与 100% 硬熔断任务失败，从不做实际压缩。
// 现由 Agent.hotPathCompactIfNeeded（agent_context_compaction.go）在每次
// LLMFillEffect 组装完 reqMsgs 后驱动：更新 currentUsage → 调用
// NeedsCompaction → 触发 internal/memory/compact 的共享 Stage1/2/3 压缩
// 算法（与 M5/网关 SessionCompressor 复用同一套算法，不重复实现）。
type ContextWindowManager struct {
	maxTokens    int // 90000
	currentUsage int
	softTrigger  float64 // 0.70
	hardTrigger  float64 // 0.90
}

// NewContextWindowManager 创建带默认阈值（0.70/0.90）的上下文窗口管理器。
// maxTokens<=0 时使用 M04 §7 默认值 90000。
func NewContextWindowManager(maxTokens int) *ContextWindowManager {
	if maxTokens <= 0 {
		maxTokens = defaultContextWindowMaxTokens
	}
	return &ContextWindowManager{
		maxTokens:   maxTokens,
		softTrigger: contextWindowSoftTrigger,
		hardTrigger: contextWindowHardTrigger,
	}
}

// SetCurrentUsage 更新当前已用 token 数（每次组装完 reqMsgs 后由调用方刷新）。
// Agent 主循环单 goroutine 串行执行，无需加锁（与 sCtx 其余字段一致的并发假设）。
func (cwm *ContextWindowManager) SetCurrentUsage(tokens int) {
	cwm.currentUsage = tokens
}

// MaxTokens 返回本管理器的上下文窗口容量（供压缩预算计算复用同一上限）。
func (cwm *ContextWindowManager) MaxTokens() int {
	return cwm.maxTokens
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
