package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

func newTodoTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("043_session_todos.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTodoRepository_ReplaceListDelete(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteTodoRepository(newTodoTestDB(t))

	if got, err := r.ListTodos(ctx, "s1"); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v %v", got, err)
	}
	a := []repo.TodoItem{{Content: "a", Status: "pending"}, {Content: "b", Status: "in_progress"}}
	if err := r.ReplaceTodos(ctx, "s1", a); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceTodos(ctx, "s2", []repo.TodoItem{{Content: "other", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	// 替换语义：新清单更短时旧行必须消失。
	if err := r.ReplaceTodos(ctx, "s1", []repo.TodoItem{{Content: "c", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.ListTodos(ctx, "s1")
	if len(got) != 1 || got[0].Content != "c" || got[0].Status != "completed" {
		t.Fatalf("replace mismatch: %+v", got)
	}
	if o, _ := r.ListTodos(ctx, "s2"); len(o) != 1 || o[0].Content != "other" {
		t.Fatalf("s2 polluted: %+v", o)
	}
	// 非法 status 由 CHECK 约束拒绝，且事务回滚保留旧清单。
	if err := r.ReplaceTodos(ctx, "s1", []repo.TodoItem{{Content: "x", Status: "bogus"}}); err == nil {
		t.Fatal("expected CHECK violation")
	}
	if got, _ := r.ListTodos(ctx, "s1"); len(got) != 1 || got[0].Content != "c" {
		t.Fatalf("rollback failed: %+v", got)
	}
	if err := r.DeleteTodos(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ListTodos(ctx, "s1"); len(got) != 0 {
		t.Fatalf("delete failed: %+v", got)
	}
	if o, _ := r.ListTodos(ctx, "s2"); len(o) != 1 {
		t.Fatal("delete leaked to s2")
	}
}

// 会话删除必须连带清理 session_todos（该表无外键级联）。
func TestChatRepo_DeleteSession_ClearsTodos(t *testing.T) {
	ctx := context.Background()
	db := newChatAppViewsTestDB(t)
	ddl, err := schema.FS.ReadFile("043_session_todos.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	chat := NewSQLiteChatRepository(db)
	if err := chat.CreateSession(ctx, types.ChatSessionRow{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	todos := NewSQLiteTodoRepository(db)
	_ = todos.ReplaceTodos(ctx, "s1", []repo.TodoItem{{Content: "a", Status: "pending"}})
	_ = todos.ReplaceTodos(ctx, "s2", []repo.TodoItem{{Content: "b", Status: "pending"}})
	if err := chat.DeleteSession(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := todos.ListTodos(ctx, "s1"); len(got) != 0 {
		t.Fatalf("s1 todos remain: %+v", got)
	}
	if got, _ := todos.ListTodos(ctx, "s2"); len(got) != 1 {
		t.Fatalf("s2 todos lost: %+v", got)
	}
}
