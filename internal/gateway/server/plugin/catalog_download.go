package plugin

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// CatalogInstaller 目录安装（marketplace.CatalogInstaller 实现）：来源取回、依赖先装、strict 组合与
// 运行时绑定都在扩展层完成，本层只触发（ADR-0103 决策二/七）。
type CatalogInstaller interface {
	Install(ctx context.Context, req marketplace.CatalogInstallRequest) (string, error)
	Upgrade(ctx context.Context, req marketplace.CatalogInstallRequest) error
	InstallFromSource(ctx context.Context, req marketplace.SourceInstallRequest) error
	Prune(ctx context.Context) ([]string, error)
}

// HandlePrunePlugins POST /v1/plugins/prune：卸载作为依赖自动安装、已无插件依赖的插件（Claude plugin prune）。
func (h *PluginHandler) HandlePrunePlugins(w http.ResponseWriter, r *http.Request) {
	if h.Catalog == nil || h.InstallMgr == nil {
		http.Error(w, "installer not initialized", http.StatusServiceUnavailable)
		return
	}
	if err := h.InstallMgr.AuthorizeAction(r.Context(), requestPrincipal(r), "plugin:manage", nil); err != nil {
		httputil.RespondError(w, "", err, http.StatusForbidden)
		return
	}
	pruned, err := h.Catalog.Prune(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if pruned == nil {
		pruned = []string{}
	}
	httputil.WriteJSON(w, map[string]any{"pruned": pruned})
}

func (h *PluginHandler) updateExtensionInstanceError(ctx context.Context, extID, errMsg string) {
	if h.InstallMgr != nil {
		if err := h.InstallMgr.UpdateInstance(ctx, extID, marketplace.InstanceUpdate{
			Status:   "error",
			ErrorMsg: errMsg,
		}); err != nil {
			slog.Warn("plugin_catalog: record extension error status failed", "ext", extID, "err", err)
		}
	}
}
