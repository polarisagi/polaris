package todo_read

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type todoOut struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// MakeTodoReadFn 读取当前会话的待办；会话 ID 缺失即报错（fail-closed）。
func MakeTodoReadFn(todos repo.TodoRepository) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if todos == nil {
			return nil, apperr.New(apperr.CodeInternal, "todo_read: 未注入 TodoRepository")
		}
		sessionID, err := guard.SessionIDFromCtx(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "todo_read", err)
		}
		items, err := todos.ListTodos(ctx, sessionID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_read: 读取失败", err)
		}
		out := make([]todoOut, 0, len(items))
		for _, it := range items {
			out = append(out, todoOut{Content: it.Content, Status: it.Status})
		}
		return json.Marshal(map[string]any{"todos": out})
	}
}
