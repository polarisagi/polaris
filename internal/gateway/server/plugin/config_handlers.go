package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// PluginConfigManager 插件 userConfig 服务（consumer-side；实现为 lifecycle.PluginConfigService）。
type PluginConfigManager interface {
	GetSchema(ctx context.Context, pluginID string) ([]lifecycle.ConfigScope, error)
	ListValues(ctx context.Context, pluginID string) ([]lifecycle.ConfigValue, error)
	SaveValues(ctx context.Context, pluginID string, updates []lifecycle.ConfigUpdate) error
}

// HandleGetPluginConfig 返回选项定义与已保存值（敏感项只报告 is_set）。
// GET /v1/plugins/{id}/config
func (h *PluginHandler) HandleGetPluginConfig(w http.ResponseWriter, r *http.Request) {
	if h.PluginConfig == nil {
		http.Error(w, "plugin config service not initialized", http.StatusServiceUnavailable)
		return
	}
	pluginID := r.PathValue("id")
	schema, err := h.PluginConfig.GetSchema(r.Context(), pluginID)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	values, err := h.PluginConfig.ListValues(r.Context(), pluginID)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSON(w, map[string]any{"plugin_id": pluginID, "scopes": schema, "values": values})
}

// HandleUpdatePluginConfig 校验并保存配置，随后重启该插件已启用的子 MCP（新值在启动时展开）。
// PUT /v1/plugins/{id}/config  body: {"values":[{"scope":"","key":"api_token","value":"..."}]}
func (h *PluginHandler) HandleUpdatePluginConfig(w http.ResponseWriter, r *http.Request) {
	if h.PluginConfig == nil || h.InstallMgr == nil {
		http.Error(w, "plugin config service not initialized", http.StatusServiceUnavailable)
		return
	}
	principal := authcontext.FromContext(r.Context()).UserID
	if principal == "" {
		principal = "user"
	}
	if err := h.InstallMgr.AuthorizeAction(r.Context(), principal, "plugin:manage", nil); err != nil {
		httputil.RespondError(w, "", err, http.StatusForbidden)
		return
	}
	var body struct {
		Values []lifecycle.ConfigUpdate `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	pluginID := r.PathValue("id")
	if err := h.PluginConfig.SaveValues(r.Context(), pluginID, body.Values); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	h.restartPluginServers(r.Context(), pluginID)
	httputil.WriteJSON(w, map[string]any{"status": "updated", "plugin_id": pluginID})
}

func (h *PluginHandler) restartPluginServers(ctx context.Context, pluginID string) {
	if h.StartMCPServer == nil {
		return
	}
	for _, serverID := range h.pluginServerIDs(ctx, pluginID, true) {
		concurrent.SafeGo(protocol.Detach(ctx), "gateway.plugin.restart_mcp_after_config", func(ctx context.Context) {
			if err := h.StartMCPServer(ctx, serverID); err != nil {
				slog.Warn("plugin_config: restart mcp server failed", "id", serverID, "err", err)
			}
		})
	}
	if h.ClearToolSchemaCache != nil {
		h.ClearToolSchemaCache()
	}
}
