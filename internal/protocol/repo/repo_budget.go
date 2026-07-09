package repo

import "context"

// BudgetRepository 预算表相关读写契约。
type BudgetRepository interface {
	GetBudget(ctx context.Context) (float64, error)
	SetBudget(ctx context.Context, monthlyUSD float64) error
}
