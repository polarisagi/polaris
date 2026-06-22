package marketplace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// HookRunner 在受限环境下执行插件 hook 脚本。
// 接口在调用方定义（AGENTS.md 原则），具体实现由 pkg/action.ContainerSandbox.RunScript 提供。
type HookRunner interface {
	// RunScript 执行 hookPath 指定的可执行文件， workDir 为工作目录。
	RunScript(ctx context.Context, hookPath, workDir string) error
}

// ExtensionInstaller 负责将扩展文件下载到本地并返回安装目录。
// 调用方注入具体实现（如 MCPMarketplaceClient.Install）。
type ExtensionInstaller interface {
	Install(ctx context.Context, target any) (installDir string, err error)
}

// RuntimeRegistrar 负责将已下载扩展注册到运行时（SkillRegistry / MCPManager）。
// 调用方按 ext_type 提供实现；nil 时跳过注册（测试 / MVP 场景）。
type RuntimeRegistrar interface {
	Register(ctx context.Context, extType, installDir, instID string) error
}

type InstallRequest struct {
	Principal   string
	ExtensionID string // Instance ID (ext_...)
	CatalogID   string
	Name        string
	ExtType     string // plugin, skill, mcp
	TrustTier   int
	Publisher   string
	HasHooks    bool
	Target      any // Catalog 查找结果，installer 用它定位下载包
	Config      string
	RuntimeID   string
	LocalPath   string // 用于本地扩展（如 generated skill）绕过下载直接指定安装路径
}

var ErrRequiresApproval = errors.New("installation requires user approval")

type Manager struct {
	extRepo           protocol.ExtensionRepository
	mcpMgr            any
	policyGate        protocol.PolicyGate
	prefsRepo         protocol.PreferencesRepo
	auditTrail        *security.AuditTrail
	publisherTrustMap map[string]int
	// hookRunner 通过 WithHookRunner 注入；nil 时 uninstall hook 降级为 warn+skip
	hookRunner HookRunner
	installer  ExtensionInstaller // 新增：文件下载
	registrar  RuntimeRegistrar   // 新增：运行时注册
}

func NewManager(extRepo protocol.ExtensionRepository, mcpMgr any, pg protocol.PolicyGate, pr protocol.PreferencesRepo, at *security.AuditTrail, publisherTrustMap map[string]int) *Manager {
	if publisherTrustMap == nil {
		publisherTrustMap = make(map[string]int)
	}
	return &Manager{
		extRepo:           extRepo,
		mcpMgr:            mcpMgr,
		policyGate:        pg,
		prefsRepo:         pr,
		auditTrail:        at,
		publisherTrustMap: publisherTrustMap,
	}
}

// WithHookRunner 注入 HookRunner 实现（如 ContainerSandbox）。返回自身支持链式调用。
func (m *Manager) WithHookRunner(hr HookRunner) *Manager {
	m.hookRunner = hr
	return m
}

func (m *Manager) WithInstaller(i ExtensionInstaller) *Manager {
	m.installer = i
	return m
}

func (m *Manager) WithRegistrar(r RuntimeRegistrar) *Manager {
	m.registrar = r
	return m
}

// Authorize handles the install flow with M11 Cedar-Gate without writing to DB.
//
//nolint:gocyclo
func (m *Manager) Authorize(ctx context.Context, req InstallRequest) error {
	mode, err := m.prefsRepo.GetPermissionMode(ctx)
	if err != nil {
		mode = types.ModeAutoReview
	}

	// 1. TrustTier Override based on whitelist
	if knownTier, ok := m.publisherTrustMap[req.Publisher]; ok {
		req.TrustTier = knownTier
	} else if req.TrustTier >= int(types.TrustOfficial) {
		req.TrustTier = int(types.TrustCommunity) // Downgrade self-claimed official
	}

	evalCtx := map[string]any{
		"trust_level":     req.TrustTier,
		"publisher":       req.Publisher,
		"ext_type":        req.ExtType,
		"permission_mode": string(mode),
		"has_hooks":       req.HasHooks,
	}

	reviewReq := types.PolicyReviewRequest{
		Principal: req.Principal,
		Action:    "install_extension",
		Resource:  req.ExtensionID,
		Context:   evalCtx,
	}

	result, err := m.policyGate.Review(ctx, reviewReq)
	if err != nil {
		return fmt.Errorf("Manager.Authorize: %w", err)
	}

	if !result.Allowed {
		if strings.HasPrefix(result.Reason, "forbidden:") {
			return apperr.New(apperr.CodeForbidden, "installation forbidden: "+result.Reason)
		}
		if result.Reason == "denied by default" {
			return apperr.Wrap(apperr.CodeForbidden, "installation requires user approval", ErrRequiresApproval)
		}
		return apperr.New(apperr.CodeForbidden, "installation denied")
	}
	return nil
}

// AuthorizeAction authorizes arbitrary actions (e.g., "plugin:manage").
func (m *Manager) AuthorizeAction(ctx context.Context, principal string, action string, target any) error {
	if m.policyGate == nil {
		return nil
	}
	reviewReq := types.PolicyReviewRequest{
		Principal: principal,
		Action:    action,
		Resource:  "plugin",
		Context:   map[string]any{},
	}
	res, err := m.policyGate.Review(ctx, reviewReq)
	if err != nil {
		return fmt.Errorf("Manager.AuthorizeAction: %w", err)
	}
	if !res.Allowed {
		return apperr.New(apperr.CodeForbidden, action+" action denied by policy")
	}
	return nil
}

// InstallExtension handles the install flow with M11 Cedar-Gate and stores to DB.
func (m *Manager) InstallExtension(ctx context.Context, req InstallRequest) error {
	if err := m.Authorize(ctx, req); err != nil {
		return fmt.Errorf("Manager.InstallExtension: %w", err)
	}

	req = normalizeInstallRequest(req)

	// 先标记 status='installing'，防止重复安装竞争（UNIQUE INDEX on catalog_id）。
	if err := m.insertExtensionInstance(ctx, req); err != nil {
		return fmt.Errorf("Manager.InstallExtension: %w", err)
	}

	return m.postInstallSteps(ctx, req)
}

func normalizeInstallRequest(req InstallRequest) InstallRequest {
	if req.ExtensionID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		req.ExtensionID = "ext_" + hex.EncodeToString(b)
	}
	if req.Name == "" {
		req.Name = req.ExtensionID
		if req.Publisher != "" {
			req.Name = fmt.Sprintf("%s/%s", req.Publisher, req.ExtensionID)
		}
	}
	if req.Config == "" {
		req.Config = "{}"
	}
	return req
}

func (m *Manager) insertExtensionInstance(ctx context.Context, req InstallRequest) error {
	err := m.extRepo.UpsertInstance(ctx, types.ExtInstanceRow{
		ID:        req.ExtensionID,
		ExtType:   req.ExtType,
		Origin:    "marketplace",
		CatalogID: req.CatalogID,
		Name:      req.Name,
		Publisher: req.Publisher,
		TrustTier: req.TrustTier,
		RuntimeID: req.RuntimeID,
		Config:    req.Config,
		Status:    "installing",
	})
	if err != nil {
		// UNIQUE 冲突 → 已安装，幂等返回成功
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil
		}
		return apperr.Wrap(apperr.CodeInternal, "marketplace.InstallExtension: insert instance", err)
	}
	return nil
}

func (m *Manager) postInstallSteps(ctx context.Context, req InstallRequest) error {
	instID := req.ExtensionID

	// 更新为 status='installed'（若非需下载类型）。
	if req.ExtType == "mcp" || req.ExtType == "" {
		err := m.extRepo.UpdateInstanceStatus(ctx, instID, "installed", "")
		if err != nil {
			slog.Warn("marketplace: failed to mark extension installed", "id", instID, "err", err)
		}
	}

	// 文件下载（若注入了 installer）或使用本地路径
	var installDir string
	if req.LocalPath != "" {
		installDir = req.LocalPath
		_ = m.extRepo.UpdateInstanceInstallPath(ctx, instID, installDir)
		_ = m.extRepo.UpdateInstanceStatus(ctx, instID, "installed", "")
	} else if m.installer != nil && req.Target != nil {
		_ = m.extRepo.UpdateInstanceStatus(ctx, instID, "downloading", "")
		dir, dlErr := m.installer.Install(ctx, req.Target)
		if dlErr != nil {
			_ = m.extRepo.UpdateInstanceStatus(ctx, instID, "failed", dlErr.Error())
			return apperr.Wrap(apperr.CodeInternal, "marketplace: download failed", dlErr)
		}
		installDir = dir
		_ = m.extRepo.UpdateInstanceInstallPath(ctx, instID, installDir)
		_ = m.extRepo.UpdateInstanceStatus(ctx, instID, "installed", "")
	}

	// 运行时注册（若注入了 registrar）
	if m.registrar != nil {
		if regErr := m.registrar.Register(ctx, req.ExtType, installDir, instID); regErr != nil {
			slog.Warn("marketplace: runtime registration failed", "inst_id", instID, "ext_type", req.ExtType, "err", regErr)
		}
	}

	return nil
}

// UninstallExtension completely removes an extension and its physical files.
//
//nolint:nestif
func (m *Manager) UninstallExtension(ctx context.Context, catalogID string) error {
	allInsts, err := m.extRepo.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("Manager.UninstallExtension: %w", err)
	}

	var insts []types.ExtInstanceRow
	for _, inst := range allInsts {
		if inst.CatalogID == catalogID {
			insts = append(insts, inst)
		}
	}

	if len(insts) == 0 {
		return apperr.New(apperr.CodeNotFound, "extension not installed")
	}

	for _, inst := range insts {
		m.removeRuntime(ctx, inst.ExtType, inst.RuntimeID, catalogID)

		if inst.InstallPath != "" {
			if inst.ExtType == "plugin" {
				var bundle protocol.PluginBundleManifest
				if raw, err := os.ReadFile(filepath.Join(inst.InstallPath, "plugin.json")); err == nil {
					_ = json.Unmarshal(raw, &bundle)
					if hook, ok := bundle.Hooks["uninstall"]; ok && hook != "" {
						hookPath := filepath.Join(inst.InstallPath, hook)
						// 路径防穿越：禁止逃逸出 installPath
						if strings.HasPrefix(filepath.Clean(hookPath), filepath.Clean(inst.InstallPath)) {
							if m.hookRunner != nil {
								// 通过注入的沙笼接口执行：具体实现由 ContainerSandbox.RunScript 提供
								if err := m.hookRunner.RunScript(ctx, hookPath, inst.InstallPath); err != nil {
									slog.Warn("marketplace: uninstall hook failed", "ext", inst.ID, "err", err)
								}
							} else {
								// hookRunner 未注入：skip，记录日志提示调用方配置 ContainerSandbox
								slog.Warn("marketplace: uninstall hook skipped (no HookRunner injected, call WithHookRunner to enable)",
									"ext", inst.ID, "hook", hookPath)
							}
						}
					}
				}
			}

			_ = os.RemoveAll(inst.InstallPath)
		}

		_ = m.extRepo.DeleteInstance(ctx, inst.ID)

		m.cleanCatalog(ctx, inst.Origin, catalogID)
	}
	return nil
}

func (m *Manager) removeRuntime(ctx context.Context, extType, runtimeID, catalogID string) {
	type mcpRemover interface{ Remove(id string) }
	switch extType {
	case "mcp":
		if remover, ok := m.mcpMgr.(mcpRemover); ok && runtimeID != "" {
			remover.Remove(runtimeID)
		}
		_ = m.extRepo.UninstallCleanup(ctx, "", runtimeID, "mcp")
	case "skill":
		// 独立安装的 skill：硬删除（非插件来源，卸载即消失）
		if runtimeID != "" {
			_ = m.extRepo.UninstallCleanup(ctx, "", runtimeID, "skill")
		}
	case "plugin":
		if runtimeID == "" {
			break
		}
		remover, _ := m.mcpMgr.(mcpRemover)
		m.removePluginRuntime(ctx, runtimeID, remover)
	case "app":
		if runtimeID != "" {
			_ = m.extRepo.UninstallCleanup(ctx, "", runtimeID, "app")
		}
	}
}

func (m *Manager) removePluginRuntime(ctx context.Context, runtimeID string, remover interface{ Remove(id string) }) {
	// 从 mcp_servers 表读取所有子 MCP ID，停止运行时连接
	if remover != nil {
		mcpRows, err := m.extRepo.ListMCPServers(ctx)
		if err == nil {
			for _, mcp := range mcpRows {
				if mcp.PluginID == runtimeID {
					remover.Remove(mcp.ID)
				}
			}
		}
	}
	// 硬删除子 MCP（卸载即消失，不留 deprecated 脏数据）
	// 硬删除子 skills（同上）
	_ = m.extRepo.UninstallCleanup(ctx, runtimeID, "", "plugin")
}

func (m *Manager) cleanCatalog(ctx context.Context, origin, catalogID string) {
	if origin == "user" {
		_ = m.extRepo.DeleteCatalogEntry(ctx, catalogID)
	} else if origin == "marketplace" {
		isBuiltin, err := m.extRepo.IsCatalogBuiltin(ctx, catalogID)
		if err == nil && !isBuiltin {
			_ = m.extRepo.DeleteCatalogEntry(ctx, catalogID)
		}
	}
}

// InstanceUpdate 表示需要更新的字段（零值字段跳过）。
type InstanceUpdate struct {
	Status      string // 若非空则更新
	RuntimeID   string // 若非空则更新
	InstallPath string // 若非空则更新
	ErrorMsg    string // 若非空则更新（"" 表示清除）
	ClearError  bool   // true 时将 error_msg 清为 NULL
}

func (m *Manager) UpdateInstance(ctx context.Context, id string, upd InstanceUpdate) error {
	if upd.Status != "" {
		if err := m.extRepo.UpdateInstanceStatus(ctx, id, upd.Status, upd.ErrorMsg); err != nil {
			return fmt.Errorf("Manager.UpdateInstance: %w", err)
		}
	} else if upd.ClearError {
		// UpdateInstanceStatus with empty errorMsg will clear it conceptually,
		// but since we don't have a direct clear method, we do a minimal update.
		_ = m.extRepo.UpdateInstanceStatus(ctx, id, "", "")
	}
	if upd.InstallPath != "" {
		if err := m.extRepo.UpdateInstanceInstallPath(ctx, id, upd.InstallPath); err != nil {
			return fmt.Errorf("Manager.UpdateInstance: %w", err)
		}
	}
	// Note: RuntimeID update is dropped as it's not present in ExtensionRepository's direct update methods,
	// but it can be handled by UpsertInstance if we really need it, though normally RuntimeID is set on install.
	return nil
}
