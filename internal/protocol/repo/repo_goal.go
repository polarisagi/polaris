package repo

import "context"

// 目标状态（053_agent_goals.sql）。
const (
	GoalActive   = "active"
	GoalPaused   = "paused"
	GoalBlocked  = "blocked"
	GoalComplete = "complete"
)

// Goal 会话级跨回合目标。
type Goal struct {
	ID           string
	SessionID    string
	Objective    string
	DoneCriteria string
	Status       string
	Note         string
	MaxRounds    int
	RoundsUsed   int
	IdleRounds   int
}

// GoalRepository 跨回合目标存储契约（ADR-0120 决策七）。
type GoalRepository interface {
	// SetGoal 新建目标并成为该会话唯一的 active 目标；已有 active 目标被置为 paused。
	// maxRounds<=0 取 20。
	SetGoal(ctx context.Context, sessionID, objective, doneCriteria string, maxRounds int) (Goal, error)
	// CurrentGoal 返回该会话最近更新的目标（任意状态）；无记录返回 (nil, nil)。
	CurrentGoal(ctx context.Context, sessionID string) (*Goal, error)
	// ActiveGoal 返回 active 目标；没有返回 (nil, nil)。
	ActiveGoal(ctx context.Context, sessionID string) (*Goal, error)
	// UpdateGoal 修改最近目标的状态与备注；status 为空保持原状态；note 为空保持原备注。
	// 无目标返回 apperr.CodeNotFound。
	UpdateGoal(ctx context.Context, sessionID, status, note string) error
	// BeginRound 为 active 目标登记一次续跑并返回更新后的目标（rounds_used+1）；
	// 目标不再 active 返回 (nil, nil)。
	BeginRound(ctx context.Context, goalID string) (*Goal, error)
	// FinishRound 登记续跑回合结果：idle=true 则 idle_rounds+1，累计 maxIdle 次自动置 paused；
	// idle=false 清零。返回更新后的目标。
	FinishRound(ctx context.Context, goalID string, idle bool, maxIdle int) (*Goal, error)
	// PauseGoal 把 active 目标置 paused（用户停止时调用）。无 active 目标静默成功。
	PauseGoal(ctx context.Context, sessionID string) error
}
