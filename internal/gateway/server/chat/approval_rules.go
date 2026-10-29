package chat

import (
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── 项目级“以后不再询问”授权规则（ADR-0117 决策五）──────────────────────────
// 读写面与项目管理同口径：本地可信客户端或 admin。

// approvalRuleCounts 各项目已记住的规则条数；失败只留痕、按 0 展示，不影响项目列表。
func (h *ChatHandler) approvalRuleCounts(r *http.Request) map[string]int {
	if h.ApprovalRules == nil {
		return nil
	}
	m, err := h.ApprovalRules.CountByProject(r.Context())
	if err != nil {
		slog.WarnContext(r.Context(), "approval rules: count by project failed", "err", err)
		return nil
	}
	return m
}

// GET /v1/approval-rules[?project_id=]
func (h *ChatHandler) HandleListApprovalRules(w http.ResponseWriter, r *http.Request) {
	if !h.requireProjectAccess(w, r) {
		return
	}
	if h.ApprovalRules == nil {
		httputil.WriteJSON(w, map[string]any{"rules": []any{}})
		return
	}
	rows, err := h.ApprovalRules.ListRules(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		respondProjectErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"rules": rows})
}

// DELETE /v1/approval-rules/{id}
func (h *ChatHandler) HandleDeleteApprovalRule(w http.ResponseWriter, r *http.Request) {
	if !h.requireProjectAccess(w, r) {
		return
	}
	if h.ApprovalRules == nil {
		respondProjectErr(w, apperr.New(apperr.CodeNotFound, "approval rule not found"))
		return
	}
	if err := h.ApprovalRules.DeleteRule(r.Context(), r.PathValue("id")); err != nil {
		respondProjectErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
