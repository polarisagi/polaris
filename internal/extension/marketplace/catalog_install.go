package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	semver "github.com/Masterminds/semver/v3"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// CatalogInstaller 从市场目录安装技能与插件（ADR-0103 决策七）。插件的依赖先于插件本身安装：
// 依赖在同一市场按名称解析；跨市场依赖受根市场 allowCrossMarketplaceDependenciesOn 约束；带版本
// 范围的依赖按 "<name>--v<version>" git 标签取满足范围的最高版本（Claude plugin dependencies）。
type CatalogInstaller struct {
	mgr     *Manager
	extRepo protocol.ExtensionRepository
	fetcher *SourceFetcher
	sync    *CatalogSync
	extDir  string
}

func NewCatalogInstaller(mgr *Manager, extRepo protocol.ExtensionRepository, fetcher *SourceFetcher, sync *CatalogSync, extDir string) *CatalogInstaller {
	return &CatalogInstaller{mgr: mgr, extRepo: extRepo, fetcher: fetcher, sync: sync, extDir: extDir}
}

// CatalogInstallRequest 一次目录安装。ExtensionID 为空时生成。
type CatalogInstallRequest struct {
	CatalogID   string
	ExtensionID string
	Principal   string
	BypassAuth  bool
}

// installCtx 一次安装（含依赖链）的共享上下文。
type installCtx struct {
	principal  string
	bypassAuth bool
	rootAllow  map[string]bool // 根插件所在市场的跨市场白名单（Claude：只有根市场的白名单生效）
	rootMkt    string
	chain      []string
}

// Install 同步执行安装（调用方自行放入后台）。返回扩展实例 ID。
func (c *CatalogInstaller) Install(ctx context.Context, req CatalogInstallRequest) (string, error) {
	row, payload, err := c.catalogEntry(ctx, req.CatalogID)
	if err != nil {
		return "", err
	}
	ic := &installCtx{principal: req.Principal, bypassAuth: req.BypassAuth, rootAllow: map[string]bool{}, rootMkt: payload.MarketplaceName}
	for _, m := range payload.AllowCrossDeps {
		ic.rootAllow[m] = true
	}
	return c.install(ctx, ic, row, payload, "", req.ExtensionID)
}

func (c *CatalogInstaller) catalogEntry(ctx context.Context, id string) (*types.ExtCatalogRow, protocol.RegistryEntry, error) {
	row, err := c.extRepo.GetCatalogEntry(ctx, id)
	if err != nil || row == nil {
		return nil, protocol.RegistryEntry{}, apperr.New(apperr.CodeNotFound, "catalog entry not found: "+id)
	}
	var payload protocol.RegistryEntry
	if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
		return nil, protocol.RegistryEntry{}, apperr.Wrap(apperr.CodeInvalidInput, "catalog entry "+id, err)
	}
	return row, payload, nil
}

func (c *CatalogInstaller) install(ctx context.Context, ic *installCtx, row *types.ExtCatalogRow, payload protocol.RegistryEntry,
	constraint, extID string,
) (string, error) {
	if extID == "" {
		extID = util.GenerateHumanReadableID("ext", payload.Name)
	}
	req := protocol.ExtensionInstallRequest{Principal: ic.principal, ExtensionID: extID, CatalogID: row.ID, Name: payload.Name,
		ExtType: row.Type, TrustTier: row.TrustTier, Publisher: row.Publisher, Config: "{}", BypassAuth: ic.bypassAuth,
		MarketplaceEntry: payload.Entry}
	if err := c.mgr.InstallExtension(ctx, req); err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller", err)
	}
	dir := filepath.Join(c.extDir, extID)
	if err := c.materialize(ctx, ic, row, payload, constraint, dir); err != nil {
		c.fail(ctx, extID, err)
		return "", err
	}
	if err := c.mgr.CompleteInstall(ctx, req, dir); err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller: complete "+payload.Name, err)
	}
	return extID, nil
}

// materialize 把扩展文件取回到 dest；插件在取回后、落运行时之前先装依赖（依赖方启用判定需要依赖已就位）。
func (c *CatalogInstaller) materialize(ctx context.Context, ic *installCtx, row *types.ExtCatalogRow, payload protocol.RegistryEntry,
	constraint, dest string,
) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CatalogInstaller: mkdir", err)
	}
	if row.Type == "skill" {
		return copyTree(payload.SourceDir, dest)
	}
	var entry pluginspec.MarketplaceEntry
	if err := json.Unmarshal(payload.Entry, &entry); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "CatalogInstaller: entry "+payload.Name, err)
	}
	src, ref, err := c.resolveVersion(ctx, row.MarketplaceID, entry, constraint)
	if err != nil {
		return err
	}
	if _, err := c.fetcher.Fetch(ctx, src, ref, dest); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller: fetch "+payload.Name, err)
	}
	return c.installDependencies(ctx, ic, dest, &entry, payload.MarketplaceName)
}

// Upgrade 按目录条目当前版本原地升级已安装实例：先取回到暂存目录（含新增依赖），成功后再替换
// 安装目录——取回失败时原安装保持不动。
func (c *CatalogInstaller) Upgrade(ctx context.Context, req CatalogInstallRequest) error {
	row, payload, err := c.catalogEntry(ctx, req.CatalogID)
	if err != nil {
		return err
	}
	ic := &installCtx{principal: req.Principal, bypassAuth: req.BypassAuth, rootAllow: map[string]bool{}, rootMkt: payload.MarketplaceName}
	for _, m := range payload.AllowCrossDeps {
		ic.rootAllow[m] = true
	}
	dest := filepath.Join(c.extDir, req.ExtensionID)
	stage, old := dest+".upgrade", dest+".previous"
	for _, d := range []string{stage, old} {
		if err := os.RemoveAll(d); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "CatalogInstaller: clean "+d, err)
		}
	}
	if err := c.materialize(ctx, ic, row, payload, "", stage); err != nil {
		os.RemoveAll(stage) //nolint:errcheck
		return err
	}
	if err := os.Rename(dest, old); err != nil && !os.IsNotExist(err) {
		return apperr.Wrap(apperr.CodeInternal, "CatalogInstaller: swap", err)
	}
	if err := os.Rename(stage, dest); err != nil {
		os.Rename(old, dest) //nolint:errcheck // 回滚到原安装
		return apperr.Wrap(apperr.CodeInternal, "CatalogInstaller: swap", err)
	}
	os.RemoveAll(old) //nolint:errcheck
	installReq := protocol.ExtensionInstallRequest{Principal: req.Principal, ExtensionID: req.ExtensionID, CatalogID: row.ID,
		Name: payload.Name, ExtType: row.Type, TrustTier: row.TrustTier, Publisher: row.Publisher, Config: "{}",
		MarketplaceEntry: payload.Entry}
	if err := c.mgr.CompleteInstall(ctx, installReq, dest); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller: upgrade "+payload.Name, err)
	}
	return nil
}

func (c *CatalogInstaller) installDependencies(ctx context.Context, ic *installCtx, dir string, entry *pluginspec.MarketplaceEntry, mkt string) error {
	spec, err := pluginspec.Load(dir, pluginspec.LoadOptions{Entry: entry, FallbackName: entry.Name})
	if err != nil {
		return nil //nolint:nilerr // 清单错误由 CompleteInstall 统一报告并置实例失败
	}
	ic.chain = append(ic.chain, mkt+"/"+entry.Name)
	defer func() { ic.chain = ic.chain[:len(ic.chain)-1] }()
	for _, dep := range spec.Dependencies {
		if err := c.installDependency(ctx, ic, dep, mkt, entry.Name); err != nil {
			return err
		}
	}
	return nil
}

func (c *CatalogInstaller) installDependency(ctx context.Context, ic *installCtx, dep pluginspec.Dependency, mkt, requiredBy string) error {
	target := firstNonEmpty(dep.Marketplace, mkt)
	key := target + "/" + dep.Name
	for _, k := range ic.chain {
		if k == key {
			return apperr.New(apperr.CodeConflict, fmt.Sprintf("circular plugin dependency: %s -> %s", strings.Join(ic.chain, " -> "), key))
		}
	}
	installed, ok, err := c.installedPlugin(ctx, dep.Name)
	if err != nil {
		return err
	}
	if ok && installed.Enabled && satisfies(dep.Version, installed.Version) {
		return nil
	}
	if target != mkt && target != ic.rootMkt && !ic.rootAllow[target] {
		// 声明在 plugin.json 中：安装继续但不装该依赖，依赖方随后按加载期检查停用（Claude 规则）。
		slog.Warn("marketplace: cross-marketplace dependency not in allowlist", "dependency", key, "required_by", requiredBy)
		return nil
	}
	row, err := c.extRepo.FindCatalogPluginByMarketplace(ctx, target, dep.Name)
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller: resolve dependency "+key, err)
	}
	if row == nil {
		slog.Warn("marketplace: dependency not found in marketplace", "dependency", key, "required_by", requiredBy)
		return nil
	}
	if ok {
		return apperr.New(apperr.CodeConflict, fmt.Sprintf("Dependency %q (required by %s) is installed at %s and does not satisfy %s; "+
			"update or remove it first", key, requiredBy, installed.Version, dep.Version))
	}
	_, payload, err := c.catalogEntry(ctx, row.ID)
	if err != nil {
		return err
	}
	if _, err := c.install(ctx, ic, row, payload, dep.Version, ""); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), fmt.Sprintf("Dependency %q (required by %s)", key, requiredBy), err)
	}
	return nil
}

func (c *CatalogInstaller) installedPlugin(ctx context.Context, name string) (types.PluginRow, bool, error) {
	rows, err := c.extRepo.ListPlugins(ctx)
	if err != nil {
		return types.PluginRow{}, false, apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller: plugins", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r, true, nil
		}
	}
	return types.PluginRow{}, false, nil
}

func satisfies(constraint, version string) bool {
	if constraint == "" {
		return true
	}
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return false
	}
	v, err := semver.NewVersion(version)
	return err == nil && c.Check(v)
}

func (c *CatalogInstaller) fail(ctx context.Context, extID string, cause error) {
	if err := c.mgr.UpdateInstance(ctx, extID, InstanceUpdate{Status: "failed", ErrorMsg: cause.Error()}); err != nil {
		slog.Warn("marketplace: record install failure failed", "ext", extID, "err", err)
	}
}
