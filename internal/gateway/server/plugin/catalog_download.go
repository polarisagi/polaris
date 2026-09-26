package plugin

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
)

// CatalogInstaller 目录安装（marketplace.CatalogInstaller 实现）：来源取回、依赖先装、strict 组合与
// 运行时绑定都在扩展层完成，本层只触发（ADR-0103 决策二/七）。
type CatalogInstaller interface {
	Install(ctx context.Context, req marketplace.CatalogInstallRequest) (string, error)
	Upgrade(ctx context.Context, req marketplace.CatalogInstallRequest) error
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
