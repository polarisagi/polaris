package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/elicitation"
)

// TestHandleGetElicitations_NotEnabled broker 未注入 → 501（同 handleGetPendingApprovals 降级方式）。
func TestHandleGetElicitations_NotEnabled(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/elicitations", nil)
	w := httptest.NewRecorder()
	s.handleGetElicitations(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

// TestHandleRespondElicitation_NotEnabled broker 未注入 → 501。
func TestHandleRespondElicitation_NotEnabled(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/v1/elicitations/x", bytes.NewBufferString(`{"action":"cancel"}`))
	req.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	s.handleRespondElicitation(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

// spawnPendingElicitation 起一个后台 Elicit 调用制造一条 pending 记录，供 handler 测试消费；
// 返回其 id 与结果 channel（调用方负责在测试结束前读取或让其超时/ctx 取消收尾）。
func spawnPendingElicitation(t *testing.T, ctx context.Context, b *elicitation.Broker, req mcp.ElicitRequest) (id string, resultCh chan mcp.ElicitResult) {
	t.Helper()
	resultCh = make(chan mcp.ElicitResult, 1)
	go func() {
		res, _ := b.Elicit(ctx, req)
		resultCh <- res
	}()
	for i := 0; i < 200; i++ {
		views := b.Pending("")
		if len(views) == 1 {
			return views[0].ID, resultCh
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pending elicitation 未按期登记")
	return "", resultCh
}

func TestHandleGetElicitations_ListsPending(t *testing.T) {
	broker := elicitation.NewBroker(nil, time.Minute)
	s := &Server{elicitationBroker: broker}

	schema := `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, resultCh := spawnPendingElicitation(t, ctx, broker, mcp.ElicitRequest{
		ServerName: "srv", SessionID: "sess-x", Mode: "form", Message: "give name",
		RequestedSchema: []byte(schema),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/elicitations?session_id=sess-x", nil)
	w := httptest.NewRecorder()
	s.handleGetElicitations(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Elicitations []elicitation.PendingView `json:"elicitations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Elicitations) != 1 || body.Elicitations[0].ID != id {
		t.Fatalf("unexpected list: %+v", body.Elicitations)
	}

	cancel()
	<-resultCh
}

func TestHandleRespondElicitation_Accept200(t *testing.T) {
	broker := elicitation.NewBroker(nil, time.Minute)
	s := &Server{elicitationBroker: broker}

	schema := `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`
	id, resultCh := spawnPendingElicitation(t, context.Background(), broker, mcp.ElicitRequest{
		ServerName: "srv", Mode: "form", Message: "give name", RequestedSchema: []byte(schema),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/elicitations/"+id, bytes.NewBufferString(`{"action":"accept","content":{"name":"alice"}}`))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.handleRespondElicitation(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	res := <-resultCh
	if res.Action != "accept" || res.Content["name"] != "alice" {
		t.Fatalf("unexpected elicit result: %+v", res)
	}
}

func TestHandleRespondElicitation_InvalidAction400(t *testing.T) {
	broker := elicitation.NewBroker(nil, time.Minute)
	s := &Server{elicitationBroker: broker}

	schema := `{"type":"object","properties":{"name":{"type":"string"}}}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, resultCh := spawnPendingElicitation(t, ctx, broker, mcp.ElicitRequest{
		ServerName: "srv", Mode: "form", Message: "m", RequestedSchema: []byte(schema),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/elicitations/"+id, bytes.NewBufferString(`{"action":"maybe"}`))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.handleRespondElicitation(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}

	cancel()
	<-resultCh
}

func TestHandleRespondElicitation_UnknownID404(t *testing.T) {
	broker := elicitation.NewBroker(nil, time.Minute)
	s := &Server{elicitationBroker: broker}

	req := httptest.NewRequest(http.MethodPost, "/v1/elicitations/does-not-exist", bytes.NewBufferString(`{"action":"cancel"}`))
	req.SetPathValue("id", "does-not-exist")
	w := httptest.NewRecorder()
	s.handleRespondElicitation(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", w.Code, w.Body.String())
	}
}
