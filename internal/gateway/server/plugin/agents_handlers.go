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

type ExpertDefinitionLister interface {
	ListExperts(ctx context.Context) ([]lifecycle.ExpertDefinition, []pluginspec.Diagnostic, error)
}

// HandleListAgents GET /v1/agents：全部可委派子 Agent 及解析诊断（含"已解析但不适用"的字段）。
func (h *PluginHandler) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	if h.AgentDefs == nil {
		httputil.RespondError(w, "agent definitions not configured", nil, http.StatusServiceUnavailable)
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

// HandleListExperts 只公开插件清单显式登记的入口，避免普通子 Agent 被误认为产品专家。
func (h *PluginHandler) HandleListExperts(w http.ResponseWriter, r *http.Request) {
	lister, ok := h.AgentDefs.(ExpertDefinitionLister)
	if !ok {
		httputil.RespondError(w, "expert definitions not configured", nil, http.StatusServiceUnavailable)
		return
	}
	experts, diags, err := lister.ListExperts(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if experts == nil {
		experts = []lifecycle.ExpertDefinition{}
	}
	if diags == nil {
		diags = []pluginspec.Diagnostic{}
	}
	httputil.WriteJSON(w, map[string]any{"experts": experts, "diagnostics": diags})
}
