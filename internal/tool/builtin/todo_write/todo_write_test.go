package todo_write_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/tool/builtin/todo_read"
	"github.com/polarisagi/polaris/internal/tool/builtin/todo_write"
)

func newRepo(t *testing.T) *repo.SQLiteTodoRepository {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, _ := schema.FS.ReadFile("043_session_todos.sql")
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return repo.NewSQLiteTodoRepository(db)
}

func sess(id string) context.Context {
	return context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, id)
}

func TestTodoTools(t *testing.T) {
	r := newRepo(t)
	w, rd := todo_write.MakeTodoWriteFn(r), todo_read.MakeTodoReadFn(r)

	// 旧字符串格式兼容 + 会话隔离
	if _, err := w(sess("s1"), []byte(`{"todos":["a","b"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := w(sess("s2"), []byte(`{"todos":[{"content":"x","status":"in_progress"}]}`)); err != nil {
		t.Fatal(err)
	}
	out, _ := rd(sess("s1"), nil)
	if string(out) != `{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"pending"}]}` {
		t.Fatalf("s1: %s", out)
	}
	out, _ = rd(sess("s2"), nil)
	if string(out) != `{"todos":[{"content":"x","status":"in_progress"}]}` {
		t.Fatalf("s2: %s", out)
	}
	// 替换语义
	_, _ = w(sess("s1"), []byte(`{"todos":[{"content":"only","status":"completed"}]}`))
	out, _ = rd(sess("s1"), nil)
	if string(out) != `{"todos":[{"content":"only","status":"completed"}]}` {
		t.Fatalf("replace: %s", out)
	}
	// 空会话读为空数组
	out, _ = rd(sess("none"), nil)
	if string(out) != `{"todos":[]}` {
		t.Fatalf("empty: %s", out)
	}
}

func TestTodoTools_MissingSessionFailsClosed(t *testing.T) {
	r := newRepo(t)
	if _, err := todo_write.MakeTodoWriteFn(r)(context.Background(), []byte(`{"todos":["a"]}`)); err == nil {
		t.Fatal("write without session must fail")
	}
	if _, err := todo_read.MakeTodoReadFn(r)(context.Background(), nil); err == nil {
		t.Fatal("read without session must fail")
	}
	if _, err := todo_write.MakeTodoWriteFn(nil)(sess("s"), []byte(`{"todos":[]}`)); err == nil {
		t.Fatal("nil repo must fail")
	}
}

func TestTodoWrite_RejectsInvalid(t *testing.T) {
	w := todo_write.MakeTodoWriteFn(newRepo(t))
	for _, in := range []string{
		`{"todos":[{"content":"a","status":"bogus"}]}`,
		`{"todos":[{"content":"","status":"pending"}]}`,
		`{"todos":[""]}`,
		`{"todos":[42]}`,
		`{"todos":"x"}`,
	} {
		if _, err := w(sess("s"), []byte(in)); err == nil {
			t.Errorf("expected error for %s", in)
		}
	}
	_, err := w(sess("s"), []byte(`{"todos":[{"content":"a","status":"bogus"}]}`))
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("unexpected: %v", err)
	}
}
