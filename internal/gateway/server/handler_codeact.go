package server

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// codeActHTTPRequest POST /v1/agent/codeact 请求体。
type codeActHTTPRequest struct {
	Language        string `json:"language"`      // "python" | "bash"
	Code            string `json:"code"`          // LLM 生成的代码
	CapabilityID    string `json:"capability_id"` // 能力令牌 ID（必填）
	SessionID       string `json:"session_id"`
	AgentID         string `json:"agent_id"`
	StatefulSession bool   `json:"stateful_session"` // GD-4-002：跨调用 REPL 状态保持
}

// codeActHTTPResponse POST /v1/agent/codeact 响应体。
type codeActHTTPResponse struct {
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
	LatencyMs int64  `json:"latency_ms"`
}

// handleCodeAct POST /v1/agent/codeact
// 同步执行 LLM 生成代码（强制 Sbx-L3），返回标准输出。
func (s *Server) handleCodeAct(w http.ResponseWriter, r *http.Request) {
	if s.codeActEngine == nil || !s.codeActEngine.CodeActAvailable() {
		err := apperr.New(apperr.CodeServiceUnavailable, "codeact engine not initialized")
		status := apperr.HTTPStatus(apperr.CodeOf(err))
		httputil.RespondError(w, "", err, status)
		return
	}

	var req codeActHTTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		err = apperr.Wrap(apperr.CodeInvalidInput, "invalid request body", err)
		httputil.RespondError(w, "invalid request body", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if req.Language == "" || req.Code == "" || req.CapabilityID == "" {
		err := apperr.New(apperr.CodeInvalidInput, "language, code, capability_id are required")
		httputil.RespondError(w, "language, code, capability_id are required", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}

	result, err := s.codeActEngine.ExecuteCode(r.Context(), protocol.CodeActRequest{
		Language:        req.Language,
		Code:            req.Code,
		CapabilityID:    req.CapabilityID,
		SessionID:       req.SessionID,
		AgentID:         req.AgentID,
		StatefulSession: req.StatefulSession,
	})
	if err != nil {
		code := apperr.CodeOf(err)
		status := apperr.HTTPStatus(code)
		httputil.RespondError(w, "", err, status)
		return
	}

	httputil.WriteJSON(w, codeActHTTPResponse{
		Output:    result.Output.UnsafeContent(), // A-4: HTTP 响应允许读取污点内容
		ExitCode:  result.ExitCode,
		LatencyMs: result.LatencyMs,
	})
}
