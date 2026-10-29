package plugin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// HandleListPluginApps GET /v1/plugins/{id}/apps：Codex 应用绑定及状态（ADR-0103 决策四）。
func (h *PluginHandler) HandleListPluginApps(w http.ResponseWriter, r *http.Request) {
	bindings, err := h.ExtRepo.ListPluginAppBindings(r.Context(), r.PathValue("id"))
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if bindings == nil {
		bindings = []types.PluginAppBinding{}
	}
	httputil.WriteJSON(w, map[string]any{"apps": bindings})
}

// HandleBindPluginApp PUT /v1/plugins/{id}/apps/{alias}：{"server_id": "..."} 绑定到本地连接器，空串解绑。
func (h *PluginHandler) HandleBindPluginApp(w http.ResponseWriter, r *http.Request) {
	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
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
		ServerID string `json:"server_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	b, err := lifecycle.BindApp(r.Context(), h.ExtRepo, r.PathValue("id"), r.PathValue("alias"), body.ServerID)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSON(w, b)
}
