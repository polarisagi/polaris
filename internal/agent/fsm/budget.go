package fsm

import "context"

// BudgetController consumer-side 接口，StateContext 通过它接入会话级预算控制。
// 具体实现 (*agent.BudgetManager) 在 agent 包中，通过编译期断言保证符合此接口。
// 接口定义在消费方（fsm 包），避免 fsm ↔ agent 循环依赖。
type BudgetController interface {
	// ConsumeTokens 记账实际消耗的 token 数，超出 Session 级预算时返回 error。
	ConsumeTokens(ctx context.Context, n int) error
	// HasSufficientBudget 推理前检查是否还有足够的预算（不扣除）。
	HasSufficientBudget(requested int) bool
	// EstimatedSpendUSD 基于已消耗 token 的近似 USD 估算，供 Cedar budget_cap 填充 monthly_spend_usd。
	EstimatedSpendUSD() float64
}
