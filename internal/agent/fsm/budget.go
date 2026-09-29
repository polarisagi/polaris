package fsm

import "context"

// BudgetController consumer-side 接口，StateContext 通过它接入会话级预算控制。
// 具体实现 (*agent.BudgetManager) 在 agent 包中，通过编译期断言保证符合此接口。
// 接口定义在消费方（fsm 包），避免 fsm ↔ agent 循环依赖。
//
// 账本为 llm_calls 表（ADR-0104 决策四）：实现无内存计数，读账本失败时软熔断放行。
type BudgetController interface {
	// ConsumeTokens 上报本次 LLM 调用的实际 token 数（仅用于埋点），并按账本判定
	// 会话累计用量是否超出 Session 级预算，超出返回 error。
	ConsumeTokens(ctx context.Context, n int) error
	// MonthlySpendUSD 当月（UTC）累计真实花费，供 Cedar budget_cap 填充 monthly_spend_usd。
	MonthlySpendUSD(ctx context.Context) float64
	// MonthlyLimitUSD 月度预算上限，0 = 不限额。每次读 BudgetRepository（短 TTL 缓存），
	// HandleSetBudget 写库后无需向 Agent 推送。
	MonthlyLimitUSD(ctx context.Context) float64
}
