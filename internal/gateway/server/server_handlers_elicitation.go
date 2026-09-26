package server

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// handleGetElicitations/handleRespondElicitation（MCP elicitation 网关侧 API，ADR-0103 决策八）
// 见本文件，风格参照 server_handlers_hitl.go 的 handleGetPendingApprovals /
// handleResolveApproval：broker 未注入时统一 501（能力未接入，非请求本身有误）。

// handleGetElicitations 列出当前挂起的 elicitation（可选 session_id 过滤）。
// GET /v1/elicitations?session_id=
func (s *Server) handleGetElicitations(w http.ResponseWriter, r *http.Request) {
	if s.elicitationBroker == nil {
		http.Error(w, "elicitation broker not enabled", http.StatusNotImplemented)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	httputil.WriteJSON(w, map[string]any{
		"elicitations": s.elicitationBroker.Pending(sessionID),
	})
}

// handleRespondElicitation 提交用户对某个 elicitation 的作答。
// POST /v1/elicitations/{id}  body: {"action":"accept|decline|cancel","content":{...}}
func (s *Server) handleRespondElicitation(w http.ResponseWriter, r *http.Request) {
	if s.elicitationBroker == nil {
		http.Error(w, "elicitation broker not enabled", http.StatusNotImplemented)
		return
	}

	var req struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}

	id := r.PathValue("id")
	if err := s.elicitationBroker.Respond(id, mcp.ElicitResult{Action: req.Action, Content: req.Content}); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSON(w, map[string]any{"status": "ok"})
}
