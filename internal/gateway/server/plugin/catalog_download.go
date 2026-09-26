package plugin

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/marketplace"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// skill/plugin 扩展的异步下载/安装 + 目录拷贝辅助（R7 拆分自 catalog_install.go）。
// MCP/generic 安装 HTTP 处理器见 catalog_install.go。
// ============================================================================

// downloadAndInstallExtension 把 skill/plugin 类扩展从本地 marketplace 缓存目录拷贝到
// extensions/{extID} 运行时目录，再交给 Manager.CompleteInstall 做运行时绑定。
// 清单与组件解析只在 extension 层（pluginspec）进行，本层不再解析任何清单格式（ADR-0103 决策二）。
func (h *PluginHandler) downloadAndInstallExtension(ctx context.Context, extID, catalogID string, installReq protocol.ExtensionInstallRequest) {
	// marketplace_id 本身可含 "/"（如 "polarisagi/polaris-plugins-official"），
	// 不能在第一个 "/" 处分割，必须从 extension_catalog 读取准确值。
	var mpID string
	if err := h.DB.QueryRowContext(ctx,
		`SELECT marketplace_id FROM extension_catalog WHERE id=?`, catalogID).Scan(&mpID); err != nil {
		h.updateExtensionInstanceError(ctx, extID, "catalog entry not found: "+err.Error())
		return
	}
	relPath := filepath.FromSlash(strings.TrimPrefix(catalogID, mpID+"/"))
	safeMpID := strings.ReplaceAll(mpID, "/", "_")
	srcDir := filepath.Join(h.DataDir, "tmp", "marketplaces", safeMpID, relPath)
	destDir := filepath.Join(h.DataDir, "extensions", extID)

	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		h.updateExtensionInstanceError(ctx, extID, err.Error())
		return
	}
	if err := copyDir(srcDir, destDir); err != nil {
		h.updateExtensionInstanceError(ctx, extID, "failed to copy from tmp: "+err.Error())
		return
	}
	if err := h.InstallMgr.CompleteInstall(ctx, installReq, destDir); err != nil {
		// FSM 已把实例置为 failed 并写入错误；此处仅留痕。
		slog.Warn("plugin_catalog: complete install failed", "ext", extID, "err", err)
	}
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

func copyDir(src string, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
	}
	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyFile", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyFile", err)
	}
	return os.WriteFile(dst, data, info.Mode()) //nolint:wrapcheck
}
