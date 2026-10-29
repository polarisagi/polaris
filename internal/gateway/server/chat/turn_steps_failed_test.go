package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestFailedTurnError(t *testing.T) {
	if failedTurnError(nil) != nil || failedTurnError(json.RawMessage(`{"steps":3}`)) != nil || failedTurnError(json.RawMessage(`not json`)) != nil {
		t.Fatal("非失败摘要应返回 nil")
	}
	got := failedTurnError(json.RawMessage(`{"steps":1,"failed":true,"error_code":"empty_response","error":"模型没有返回内容","error_detail":"d"}`))
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil || m["code"] != "empty_response" || m["message"] != "模型没有返回内容" || m["detail"] != "d" {
		t.Fatalf("got %s (%v)", got, err)
	}
}

// 历史接口：失败回合的用户消息带 turn_error，成功回合的用户消息不带；失败占位只来自步骤摘要，不是 chat_messages 内容。
func TestHandleGetSession_FailedTurnPlaceholder(t *testing.T) {
	db := newSessionsMCPAppsTestDB(t)
	chatRepo := repo.NewSQLiteChatRepository(db)
	turnRepo := repo.NewSQLiteChatTurnRepository(db, db)
	ctx := context.Background()
	if err := chatRepo.CreateSession(ctx, types.ChatSessionRow{ID: "sess-f"}); err != nil {
		t.Fatal(err)
	}
	failedUser, _ := chatRepo.AppendMessage(ctx, types.ChatMessageRow{SessionID: "sess-f", Role: "user", Content: "第一次提问"})
	okUser, _ := chatRepo.AppendMessage(ctx, types.ChatMessageRow{SessionID: "sess-f", Role: "user", Content: "第二次提问"})
	if _, err := chatRepo.AppendMessage(ctx, types.ChatMessageRow{SessionID: "sess-f", Role: "assistant", Content: "答复"}); err != nil {
		t.Fatal(err)
	}
	// 失败回合：摘要行 failed=true；用户消息被挂上回合 ID。
	failedSummary := []byte(`{"steps":1,"failed":true,"error_code":"empty_response","error":"模型没有返回内容","error_detail":"推理返回空内容"}`)
	if err := turnRepo.AppendTurnSteps(ctx, []protorepo.TurnStepRow{{TurnID: "turn_f", SessionID: "sess-f", Seq: 0, Kind: "summary", Data: failedSummary, CreatedAtMs: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := turnRepo.AttachTurnMeta(ctx, failedUser, "turn_f", ""); err != nil {
		t.Fatal(err)
	}
	// 成功回合的用户消息即使被误挂回合 ID，摘要未标记失败也不应出现占位。
	if err := turnRepo.AppendTurnSteps(ctx, []protorepo.TurnStepRow{{TurnID: "turn_ok", SessionID: "sess-f", Seq: 0, Kind: "summary", Data: []byte(`{"steps":2}`), CreatedAtMs: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := turnRepo.AttachTurnMeta(ctx, okUser, "turn_ok", ""); err != nil {
		t.Fatal(err)
	}

	h := &ChatHandler{TurnRepo: turnRepo, PersistenceService: &ChatPersistenceService{DB: db, ChatRepo: chatRepo}}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-f", nil)
	req.SetPathValue("sessionID", "sess-f")
	rec := httptest.NewRecorder()
	h.HandleGetSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Messages []struct {
			Role      string          `json:"role"`
			Content   string          `json:"content"`
			TurnID    string          `json:"turn_id"`
			TurnError json.RawMessage `json:"turn_error"`
			Steps     json.RawMessage `json:"steps"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("消息数应保持 3（占位不是消息）: %d", len(body.Messages))
	}
	var te map[string]string
	if err := json.Unmarshal(body.Messages[0].TurnError, &te); err != nil || te["message"] != "模型没有返回内容" || body.Messages[0].TurnID != "turn_f" {
		t.Fatalf("失败回合的用户消息应带 turn_error: %+v", body.Messages[0])
	}
	if body.Messages[0].Steps != nil {
		t.Errorf("用户消息不应带步骤摘要: %s", body.Messages[0].Steps)
	}
	if body.Messages[1].TurnError != nil || body.Messages[1].TurnID != "" {
		t.Errorf("成功回合的用户消息不应带占位信息: %+v", body.Messages[1])
	}
}
