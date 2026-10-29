package plugin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// PluginChannelManager Claude 插件 channels 的启用管理（lifecycle.ChannelService 实现）。
type PluginChannelManager interface {
	ListBindings(ctx context.Context) ([]lifecycle.ChannelBinding, error)
	SetBindingState(ctx context.Context, st types.PluginChannelState) error
}

// HandleListPluginChannels GET /v1/plugins/channels：插件声明的 channel 及启用/能力状态。
func (h *PluginHandler) HandleListPluginChannels(w http.ResponseWriter, r *http.Request) {
	if h.Channels == nil {
		http.Error(w, "plugin channels not configured", http.StatusServiceUnavailable)
		return
	}
	bindings, err := h.Channels.ListBindings(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if bindings == nil {
		bindings = []lifecycle.ChannelBinding{}
	}
	httputil.WriteJSON(w, map[string]any{"channels": bindings})
}

// HandleSetPluginChannel PUT /v1/plugins/channels：显式启用 channel / 审批转发（安装 ≠ 启用）。
// 启用会让外部消息进入 Agent 会话、转发会让远程通道裁决审批，须 plugin:manage 授权。
func (h *PluginHandler) HandleSetPluginChannel(w http.ResponseWriter, r *http.Request) {
	if h.Channels == nil || h.InstallMgr == nil {
		http.Error(w, "plugin channels not configured", http.StatusServiceUnavailable)
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
		PluginID        string `json:"plugin_id"`
		Server          string `json:"server"`
		Enabled         bool   `json:"enabled"`
		PermissionRelay bool   `json:"permission_relay"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PluginID == "" || body.Server == "" {
		http.Error(w, "plugin_id and server required", http.StatusBadRequest)
		return
	}
	st := types.PluginChannelState{PluginID: body.PluginID, Server: body.Server, Enabled: body.Enabled,
		PermissionRelay: body.Enabled && body.PermissionRelay}
	if err := h.Channels.SetBindingState(r.Context(), st); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSON(w, map[string]any{"status": "ok", "enabled": st.Enabled, "permission_relay": st.PermissionRelay})
}
