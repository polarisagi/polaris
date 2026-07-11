package server

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// codeActHTTPRequest POST /v1/agent/codeact 请求体。
type codeActHTTPRequest struct {
	Language        string           `json:"language"`      // "python" | "bash"
	Code            string           `json:"code"`          // LLM 生成的代码
	CapabilityID    string           `json:"capability_id"` // 能力令牌 ID（必填）
	SessionID       string           `json:"session_id"`
	AgentID         string           `json:"agent_id"`
	TaintLevel      types.TaintLevel `json:"taint_level"`
	StatefulSession bool             `json:"stateful_session"` // GD-4-002：跨调用 REPL 状态保持
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
		http.Error(w, `{"error":"codeact engine not initialized"}`, http.StatusServiceUnavailable)
		return
	}

	var req codeActHTTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Language == "" || req.Code == "" || req.CapabilityID == "" {
		http.Error(w, `{"error":"language, code, capability_id are required"}`, http.StatusBadRequest)
		return
	}

	result, err := s.codeActEngine.ExecuteCode(r.Context(), protocol.CodeActRequest{
		Language:        req.Language,
		Code:            req.Code,
		CapabilityID:    req.CapabilityID,
		SessionID:       req.SessionID,
		AgentID:         req.AgentID,
		TaintLevel:      req.TaintLevel,
		StatefulSession: req.StatefulSession,
	})
	if err != nil {
		code := apperr.CodeOf(err)
		status := apperr.HTTPStatus(code)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(codeActHTTPResponse{
		Output:    string(result.Output),
		ExitCode:  result.ExitCode,
		LatencyMs: result.LatencyMs,
	})
}
