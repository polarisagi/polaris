package repo

import "context"

// TurnStepRow 是 chat_turn_steps 的一行（013_chat.sql，ADR-0118）。Seq=0 是该回合的摘要行（Kind=summary）。
type TurnStepRow struct {
	TurnID      string
	SessionID   string
	Seq         int
	ParentID    string
	Kind        string
	Data        []byte // 步骤 JSON
	CreatedAtMs int64
}

// ChatTurnRepository 回合步骤与消息回合元数据的持久化。
//
// @producer: internal/store/repo/repo_chat_turns.go
type ChatTurnRepository interface {
	// AppendTurnSteps 批量写入（同 turn_id+seq 覆盖），单事务。
	AppendTurnSteps(ctx context.Context, rows []TurnStepRow) error
	// ListTurnSteps 返回一个回合的全部行（含摘要行），按 seq 升序。session 不匹配的 turn 视为不存在。
	ListTurnSteps(ctx context.Context, sessionID, turnID string) ([]TurnStepRow, error)
	// ListSessionToolSteps 返回会话内全部 kind=tool 的步骤行，按 turn_id、seq 升序（回合摘要用，ADR-0120 决策四）。
	ListSessionToolSteps(ctx context.Context, sessionID string) ([]TurnStepRow, error)
	// TurnSummaries 返回会话内每个回合的摘要行 JSON（turn_id → data），供会话加载一次取齐。
	TurnSummaries(ctx context.Context, sessionID string) (map[string][]byte, error)
	// AttachTurnMeta 给 assistant 消息写回合 ID 与用量摘要 JSON。
	AttachTurnMeta(ctx context.Context, messageID int64, turnID, usageJSON string) error
	// PurgeTurnStepsBefore 删除 created_at < beforeMs 的步骤行（"步骤保留天数"清理），返回删除行数。
	PurgeTurnStepsBefore(ctx context.Context, beforeMs int64) (int64, error)
}
