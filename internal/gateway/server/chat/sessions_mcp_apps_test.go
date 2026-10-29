package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// newSessionsMCPAppsTestDB 用真实 013_chat.sql 建库（含 chat_app_views 表），
// 与 sessions_helpers_test.go newTestChatDB 的手抄 schema 不同——本测试专门
// 验证 chat_app_views 联表读取，必须用真实 DDL（见 repo 层同类测试注释）。
func newSessionsMCPAppsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	ddl, err := schema.FS.ReadFile("013_chat.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestHandleGetSession_ReturnsAppViewsForAssistantMessage 验证 M8f-1：会话历史
// 接口按 assistant 消息返回其关联的 MCP Apps 视图，刷新页面后可重新渲染。
func TestHandleGetSession_ReturnsAppViewsForAssistantMessage(t *testing.T) {
	db := newSessionsMCPAppsTestDB(t)
	chatRepo := repo.NewSQLiteChatRepository(db)
	ctx := context.Background()

	if err := chatRepo.CreateSession(ctx, types.ChatSessionRow{ID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := chatRepo.AppendMessage(ctx, types.ChatMessageRow{SessionID: "sess-1", Role: "user", Content: "refresh the dashboard"}); err != nil {
		t.Fatal(err)
	}
	assistantID, err := chatRepo.AppendMessage(ctx, types.ChatMessageRow{SessionID: "sess-1", Role: "assistant", Content: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := chatRepo.SaveAppView(ctx, types.ChatAppViewRow{
		ViewID: "view-1", SessionID: "sess-1", ServerID: "srv1",
		ResourceURI: "ui://srv1/dash", ToolName: "dash",
		ToolInput: `{"x":1}`, ToolResult: `{"content":[]}`, WidgetState: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := chatRepo.LinkAppViewsToMessage(ctx, "sess-1", []string{"view-1"}, assistantID); err != nil {
		t.Fatal(err)
	}

	h := &ChatHandler{PersistenceService: &ChatPersistenceService{DB: db, ChatRepo: chatRepo}}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1", nil)
	req.SetPathValue("sessionID", "sess-1")
	rec := httptest.NewRecorder()
	h.HandleGetSession(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Messages []struct {
			Role  string `json:"role"`
			Views []struct {
				ViewID      string `json:"view_id"`
				ServerID    string `json:"server_id"`
				ResourceURI string `json:"resource_uri"`
			} `json:"views"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, rec.Body.String())
	}
	if len(body.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(body.Messages))
	}
	if len(body.Messages[0].Views) != 0 {
		t.Fatalf("user message must not carry views, got %+v", body.Messages[0].Views)
	}
	if len(body.Messages[1].Views) != 1 || body.Messages[1].Views[0].ViewID != "view-1" ||
		body.Messages[1].Views[0].ServerID != "srv1" || body.Messages[1].Views[0].ResourceURI != "ui://srv1/dash" {
		t.Fatalf("expected assistant message to carry view-1, got %+v", body.Messages[1].Views)
	}
}
