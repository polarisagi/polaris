package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

// newChatAppViewsTestDB 用真实 013_chat.sql（schema SSoT）建库，见
// repo_project_test.go newProjectTestDB 同一理由。
func newChatAppViewsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	ddl, err := schema.FS.ReadFile("013_chat.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply 013_chat.sql: %v", err)
	}
	return db
}

func TestChatRepo_AppendMessage_ReturnsInsertedID(t *testing.T) {
	r := NewSQLiteChatRepository(newChatAppViewsTestDB(t))
	ctx := context.Background()
	if err := r.CreateSession(ctx, types.ChatSessionRow{ID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	id1, err := r.AppendMessage(ctx, types.ChatMessageRow{SessionID: "sess-1", Role: "user", Content: "hi"})
	if err != nil || id1 <= 0 {
		t.Fatalf("id1=%d err=%v", id1, err)
	}
	id2, err := r.AppendMessage(ctx, types.ChatMessageRow{SessionID: "sess-1", Role: "assistant", Content: "hello"})
	if err != nil || id2 <= id1 {
		t.Fatalf("id2=%d must be > id1=%d, err=%v", id2, id1, err)
	}
}

func TestChatRepo_AppView_SaveGetLinkList(t *testing.T) {
	r := NewSQLiteChatRepository(newChatAppViewsTestDB(t))
	ctx := context.Background()
	if err := r.CreateSession(ctx, types.ChatSessionRow{ID: "sess-1"}); err != nil {
		t.Fatal(err)
	}

	view := types.ChatAppViewRow{
		ViewID: "view-1", SessionID: "sess-1", ServerID: "srv1",
		ResourceURI: "ui://srv1/dash", ToolName: "dash",
		ToolInput: `{"x":1}`, ToolResult: `{"content":[]}`, WidgetState: "{}",
	}
	if err := r.SaveAppView(ctx, view); err != nil {
		t.Fatal(err)
	}

	got, err := r.GetAppView(ctx, "view-1")
	if err != nil || got == nil {
		t.Fatalf("GetAppView: %v %v", got, err)
	}
	if got.MessageID != nil {
		t.Fatalf("expected nil MessageID before linking, got %v", *got.MessageID)
	}
	if got.ServerID != "srv1" || got.ResourceURI != "ui://srv1/dash" {
		t.Fatalf("unexpected view: %+v", got)
	}

	msgID, err := r.AppendMessage(ctx, types.ChatMessageRow{SessionID: "sess-1", Role: "assistant", Content: "here"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.LinkAppViewsToMessage(ctx, "sess-1", []string{"view-1", "missing-view"}, msgID); err != nil {
		t.Fatal(err)
	}

	linked, err := r.GetAppView(ctx, "view-1")
	if err != nil || linked == nil || linked.MessageID == nil || *linked.MessageID != msgID {
		t.Fatalf("expected view linked to message %d, got %+v (%v)", msgID, linked, err)
	}

	byMsg, err := r.ListAppViewsByMessageIDs(ctx, []int64{msgID})
	if err != nil || len(byMsg) != 1 || byMsg[0].ViewID != "view-1" {
		t.Fatalf("ListAppViewsByMessageIDs: %+v %v", byMsg, err)
	}

	// 不存在的 view_id 不影响其它行的 link（"missing-view" 静默跳过）。
	if err := r.UpdateAppViewWidgetState(ctx, "view-1", `{"tab":"a"}`); err != nil {
		t.Fatal(err)
	}
	afterState, _ := r.GetAppView(ctx, "view-1")
	if afterState.WidgetState != `{"tab":"a"}` {
		t.Fatalf("widget_state not updated: %q", afterState.WidgetState)
	}

	if err := r.UpdateAppViewWidgetState(ctx, "does-not-exist", `{}`); err == nil {
		t.Fatal("expected NotFound updating widget_state for unknown view")
	}
}

func TestChatRepo_SessionModelContext_UpsertAndConsume(t *testing.T) {
	r := NewSQLiteChatRepository(newChatAppViewsTestDB(t))
	ctx := context.Background()
	if err := r.CreateSession(ctx, types.ChatSessionRow{ID: "sess-1"}); err != nil {
		t.Fatal(err)
	}

	if err := r.UpsertSessionModelContextServer(ctx, "sess-1", "srv1", `{"content":"first"}`); err != nil {
		t.Fatal(err)
	}
	// 同一 (session, server) 第二次更新覆盖第一次（"只保留最新一份"）。
	if err := r.UpsertSessionModelContextServer(ctx, "sess-1", "srv1", `{"content":"second"}`); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertSessionModelContextServer(ctx, "sess-1", "srv2", `{"content":"other-server"}`); err != nil {
		t.Fatal(err)
	}

	raw, err := r.ConsumeSessionModelContext(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if raw == "{}" || raw == "" {
		t.Fatalf("expected non-empty pending context, got %q", raw)
	}
	// srv1 键必须是 second（覆盖），srv2 键独立保留。
	if !containsJSONFragment(raw, `"srv1":{"content":"second"}`) {
		t.Fatalf("expected srv1 overwritten to second: %q", raw)
	}
	if !containsJSONFragment(raw, `"srv2":{"content":"other-server"}`) {
		t.Fatalf("expected srv2 preserved: %q", raw)
	}

	// 消费后清空（regulation: 只把最后一次更新送给模型，注入后即清空）。
	raw2, err := r.ConsumeSessionModelContext(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if raw2 != "{}" {
		t.Fatalf("expected cleared after consume, got %q", raw2)
	}
}

func containsJSONFragment(raw, fragment string) bool {
	return len(raw) > 0 && (indexOf(raw, fragment) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
