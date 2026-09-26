package plugin

import (
	"context"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// AgentDefinitionLister 子 Agent 定义列表（lifecycle.AgentDefinitionProvider 实现）。
type AgentDefinitionLister interface {
	ListAgentDefinitions(ctx context.Context) ([]lifecycle.AgentDefinition, []pluginspec.Diagnostic, error)
}

// HandleListAgents GET /v1/agents：全部可委派子 Agent 及解析诊断（含"已解析但不适用"的字段）。
func (h *PluginHandler) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	if h.AgentDefs == nil {
		http.Error(w, "agent definitions not configured", http.StatusServiceUnavailable)
		return
	}
	defs, diags, err := h.AgentDefs.ListAgentDefinitions(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if defs == nil {
		defs = []lifecycle.AgentDefinition{}
	}
	httputil.WriteJSON(w, map[string]any{"agents": defs, "diagnostics": diags})
}
