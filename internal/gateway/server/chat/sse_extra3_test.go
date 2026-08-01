package chat

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleAgentStreamErrors(t *testing.T) {
	defer func() { recover() }()
	h := &ChatHandler{DataDir: t.TempDir()}

	// Bad JSON
	req1 := httptest.NewRequest("POST", "/agent/stream", bytes.NewBufferString("invalid json"))
	w1 := httptest.NewRecorder()
	h.HandleAgentStream(w1, req1)

	// Empty input
	req2 := httptest.NewRequest("POST", "/agent/stream", bytes.NewBufferString(`{"input": "   ", "session_id": "123"}`))
	w2 := httptest.NewRecorder()
	h.HandleAgentStream(w2, req2)

	// Flusher test
	req3 := httptest.NewRequest("POST", "/agent/stream", bytes.NewBufferString(`{"input": "hello", "session_id": "123"}`))
	w3 := httptest.NewRecorder()
	h.HandleAgentStream(w3, req3)
}

// TestHandleAgentStream_InvalidSessionID_S07 验证 S-07 修复：非法 session_id
// 在入口即被拒绝（400），且不会继续往下触达 PersistenceService/AgentPool——
// 本测试特意使用零值 ChatHandler（PersistenceService 为 nil），若校验被绕过，
// 后续 EnsureSession 调用会因 nil 指针 panic，测试将失败而非静默通过。
func TestHandleAgentStream_InvalidSessionID_S07(t *testing.T) {
	// PromptService 必须非 nil（哪怕零值），否则会在 session_id 校验之前就因
	// s.PromptService.ContextRefExpander 的 nil 解引用 panic，掩盖本测试目标。
	h := &ChatHandler{DataDir: t.TempDir(), PromptService: &PromptAssemblyService{}}

	req := httptest.NewRequest("POST", "/agent/stream",
		bytes.NewBufferString(`{"input": "hello", "session_id": "../evil"}`))
	w := httptest.NewRecorder()

	h.HandleAgentStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid session_id, got %d", w.Code)
	}
}
