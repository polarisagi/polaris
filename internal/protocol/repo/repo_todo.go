package repo

import "context"

// TodoItem 会话待办的一项（043_session_todos.sql）。Status ∈ pending|in_progress|completed。
type TodoItem struct {
	Content string
	Status  string
}

// TodoRepository 会话级待办存储契约（ADR-0104 决策三）。
type TodoRepository interface {
	// ReplaceTodos 单事务先删后插，整表替换该会话的清单；items 为空即清空。
	ReplaceTodos(ctx context.Context, sessionID string, items []TodoItem) error
	// ListTodos 按 seq 升序返回；无记录返回空切片。
	ListTodos(ctx context.Context, sessionID string) ([]TodoItem, error)
	// DeleteTodos 删除该会话全部待办。
	DeleteTodos(ctx context.Context, sessionID string) error
}
