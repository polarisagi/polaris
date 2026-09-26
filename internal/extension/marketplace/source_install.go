package marketplace

import (
	"context"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SourceInstallRequest 用户直接给出来源（不经市场目录）的插件/技能安装。
type SourceInstallRequest struct {
	ExtensionID string
	Name        string
	ExtType     string // plugin / skill
	Principal   string
	TrustTier   int
	BypassAuth  bool
	Source      pluginspec.PluginSource
}

// InstallFromSource 取回来源并安装。直接来源不属于任何市场：插件依赖只能匹配已安装插件，缺失时
// 按加载期检查停用（Claude：--plugin-dir 等非市场插件的依赖须先行安装）。
func (c *CatalogInstaller) InstallFromSource(ctx context.Context, req SourceInstallRequest) error {
	if req.ExtType != "plugin" && req.ExtType != "skill" {
		return apperr.New(apperr.CodeInvalidInput, "InstallFromSource: unsupported type "+req.ExtType)
	}
	installReq := protocol.ExtensionInstallRequest{Principal: req.Principal, ExtensionID: req.ExtensionID, Name: req.Name,
		ExtType: req.ExtType, TrustTier: req.TrustTier, Publisher: "user", Config: "{}", BypassAuth: req.BypassAuth}
	if err := c.mgr.InstallExtension(ctx, installReq); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "InstallFromSource", err)
	}
	dest := filepath.Join(c.extDir, req.ExtensionID)
	if _, err := c.fetcher.Fetch(ctx, req.Source, "", dest); err != nil {
		c.fail(ctx, req.ExtensionID, err)
		return apperr.Wrap(apperr.CodeOf(err), "InstallFromSource: fetch", err)
	}
	if err := c.mgr.CompleteInstall(ctx, installReq, dest); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "InstallFromSource: complete "+req.Name, err)
	}
	return nil
}
