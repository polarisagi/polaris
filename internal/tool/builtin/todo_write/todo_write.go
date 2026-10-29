package todo_write

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// MakeTodoWriteFn 按会话整表替换待办（ADR-0104 决策三）。会话 ID 缺失即报错，
// 不回落共享存储。
func MakeTodoWriteFn(todos repo.TodoRepository) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if todos == nil {
			return nil, apperr.New(apperr.CodeInternal, "todo_write: 未注入 TodoRepository")
		}
		sessionID, err := guard.SessionIDFromCtx(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "todo_write", err)
		}
		var args struct {
			Todos []json.RawMessage `json:"todos"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "todo_write: invalid args", err)
		}
		items, err := parseItems(args.Todos)
		if err != nil {
			return nil, err
		}
		if err := todos.ReplaceTodos(ctx, sessionID, items); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_write: 落库失败", err)
		}
		return []byte(`{"status":"success"}`), nil
	}
}

// parseItems 兼容旧格式（字符串，status=pending）与 {content,status} 对象。
func parseItems(raws []json.RawMessage) ([]repo.TodoItem, error) {
	items := make([]repo.TodoItem, 0, len(raws))
	for i, raw := range raws {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if strings.TrimSpace(s) == "" {
				return nil, apperr.New(apperr.CodeInvalidInput, "todo_write: 待办内容不能为空")
			}
			items = append(items, repo.TodoItem{Content: s, Status: "pending"})
			continue
		}
		var o struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "todo_write: 第 "+itoa(i)+" 项既不是字符串也不是对象", err)
		}
		if strings.TrimSpace(o.Content) == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "todo_write: 待办内容不能为空")
		}
		if o.Status == "" {
			o.Status = "pending"
		}
		switch o.Status {
		case "pending", "in_progress", "completed":
		default:
			return nil, apperr.New(apperr.CodeInvalidInput, "todo_write: 非法 status "+o.Status+"（允许 pending|in_progress|completed）")
		}
		items = append(items, repo.TodoItem{Content: o.Content, Status: o.Status})
	}
	return items, nil
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
