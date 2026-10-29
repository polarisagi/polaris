package plugin

import (
	"context"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// PluginDependencyManager 插件依赖检查（lifecycle.PluginDependencies 实现）。
type PluginDependencyManager interface {
	Check(ctx context.Context, pluginID string) ([]lifecycle.DependencyStatus, error)
	EnableBlocker(ctx context.Context, pluginID string) error
	EnforceAll(ctx context.Context) ([]string, error)
}

// HandleListPluginDependencies GET /v1/plugins/{id}/dependencies：依赖逐条状态（ok/missing/disabled/version_mismatch）。
func (h *PluginHandler) HandleListPluginDependencies(w http.ResponseWriter, r *http.Request) {
	if h.Dependencies == nil {
		http.Error(w, "plugin dependencies not configured", http.StatusServiceUnavailable)
		return
	}
	statuses, err := h.Dependencies.Check(r.Context(), r.PathValue("id"))
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if statuses == nil {
		statuses = []lifecycle.DependencyStatus{}
	}
	httputil.WriteJSON(w, map[string]any{"dependencies": statuses})
}
