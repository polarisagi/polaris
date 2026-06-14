package marketplace

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
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
}

var ErrRequiresApproval = errors.New("installation requires user approval")

type Manager struct {
	db                *sql.DB
	mcpMgr            any
	policyGate        protocol.PolicyGate
	prefsRepo         protocol.PreferencesRepo
	auditTrail        *substrate.AuditTrail
	publisherTrustMap map[string]int
	// hookRunner 通过 WithHookRunner 注入；nil 时 uninstall hook 降级为 warn+skip
	hookRunner HookRunner
	installer  ExtensionInstaller // 新增：文件下载
	registrar  RuntimeRegistrar   // 新增：运行时注册
}

func NewManager(db *sql.DB, mcpMgr any, pg protocol.PolicyGate, pr protocol.PreferencesRepo, at *substrate.AuditTrail, publisherTrustMap map[string]int) *Manager {
	if publisherTrustMap == nil {
		publisherTrustMap = make(map[string]int)
	}
	return &Manager{
		db:                db,
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
		mode = protocol.ModeAutoReview
	}

	// 1. TrustTier Override based on whitelist
	if knownTier, ok := m.publisherTrustMap[req.Publisher]; ok {
		req.TrustTier = knownTier
	} else if req.TrustTier >= int(protocol.TrustOfficial) {
		req.TrustTier = int(protocol.TrustCommunity) // Downgrade self-claimed official
	}

	evalCtx := map[string]any{
		"trust_level":     req.TrustTier,
		"publisher":       req.Publisher,
		"ext_type":        req.ExtType,
		"permission_mode": string(mode),
		"has_hooks":       req.HasHooks,
	}

	reviewReq := protocol.PolicyReviewRequest{
		Principal: req.Principal,
		Action:    "install_extension",
		Resource:  req.ExtensionID,
		Context:   evalCtx,
	}

	result, err := m.policyGate.Review(ctx, reviewReq)
	if err != nil {
		return err
	}

	if !result.Allowed {
		if strings.HasPrefix(result.Reason, "forbidden:") {
			return perrors.New(perrors.CodeForbidden, "installation forbidden: "+result.Reason)
		}
		if result.Reason == "denied by default" {
			return ErrRequiresApproval
		}
		return perrors.New(perrors.CodeForbidden, "installation denied")
	}
	return nil
}

// AuthorizeAction authorizes arbitrary actions (e.g., "plugin:manage").
func (m *Manager) AuthorizeAction(ctx context.Context, action string, target any) error {
	if m.policyGate == nil {
		return nil
	}
	reviewReq := protocol.PolicyReviewRequest{
		Principal: "system",
		Action:    action,
		Resource:  "plugin",
		Context:   map[string]any{},
	}
	res, err := m.policyGate.Review(ctx, reviewReq)
	if err != nil {
		return err
	}
	if !res.Allowed {
		return perrors.New(perrors.CodeForbidden, action+" action denied by policy")
	}
	return nil
}

// InstallExtension handles the install flow with M11 Cedar-Gate and stores to DB.
func (m *Manager) InstallExtension(ctx context.Context, req InstallRequest) error {
	if err := m.Authorize(ctx, req); err != nil {
		return err
	}

	req = normalizeInstallRequest(req)

	// 先标记 status='installing'，防止重复安装竞争（UNIQUE INDEX on catalog_id）。
	if err := m.insertExtensionInstance(ctx, req); err != nil {
		return err
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
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO extension_instances
			(id, ext_type, origin, catalog_id, name, publisher, trust_tier, runtime_id, config, status, created_at, updated_at)
		VALUES (?, ?, 'marketplace', ?, ?, ?, ?, ?, ?, 'installing', ?, ?)`,
		req.ExtensionID, req.ExtType, req.CatalogID, req.Name, req.Publisher, req.TrustTier, req.RuntimeID, req.Config, now, now,
	)
	if err != nil {
		// UNIQUE 冲突 → 已安装，幂等返回成功
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil
		}
		return perrors.Wrap(perrors.CodeInternal, "marketplace.InstallExtension: insert instance", err)
	}
	return nil
}

func (m *Manager) postInstallSteps(ctx context.Context, req InstallRequest) error {
	instID := req.ExtensionID

	// 更新为 status='installed'（若非需下载类型）。
	if req.ExtType == "mcp" || req.ExtType == "" {
		_, err := m.db.ExecContext(ctx, `UPDATE extension_instances SET status='installed' WHERE id=?`, instID)
		if err != nil {
			slog.Warn("marketplace: failed to mark extension installed", "id", instID, "err", err)
		}
	}

	// 文件下载（若注入了 installer）
	var installDir string
	if m.installer != nil && req.Target != nil {
		_, _ = m.db.ExecContext(ctx, `UPDATE extension_instances SET status='downloading' WHERE id=?`, instID)
		dir, dlErr := m.installer.Install(ctx, req.Target)
		if dlErr != nil {
			_, _ = m.db.ExecContext(ctx, `UPDATE extension_instances SET status='failed' WHERE id=?`, instID)
			return perrors.Wrap(perrors.CodeInternal, "marketplace: download failed", dlErr)
		}
		installDir = dir
		_, _ = m.db.ExecContext(ctx, `UPDATE extension_instances SET status='installed', install_path=? WHERE id=?`, installDir, instID)
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
	rows, err := m.db.QueryContext(ctx, `
		SELECT id, ext_type, runtime_id, install_path, origin
		FROM extension_instances
		WHERE catalog_id=?`, catalogID)
	if err != nil {
		return err
	}
	defer rows.Close()

	type instRow struct {
		id, extType, runtimeID, installPath, origin string
	}
	var insts []instRow
	for rows.Next() {
		var inst instRow
		if err := rows.Scan(&inst.id, &inst.extType, &inst.runtimeID, &inst.installPath, &inst.origin); err == nil {
			insts = append(insts, inst)
		}
	}

	if len(insts) == 0 {
		return perrors.New(perrors.CodeNotFound, "extension not installed")
	}

	for _, inst := range insts {
		m.removeRuntime(ctx, inst.extType, inst.runtimeID, catalogID)

		if inst.installPath != "" {
			if inst.extType == "plugin" {
				var bundle protocol.PluginBundleManifest
				if raw, err := os.ReadFile(filepath.Join(inst.installPath, "plugin.json")); err == nil {
					_ = json.Unmarshal(raw, &bundle)
					if hook, ok := bundle.Hooks["uninstall"]; ok && hook != "" {
						hookPath := filepath.Join(inst.installPath, hook)
						// 路径防穿越：禁止逃逸出 installPath
						if strings.HasPrefix(filepath.Clean(hookPath), filepath.Clean(inst.installPath)) {
							if m.hookRunner != nil {
								// 通过注入的沙笼接口执行：具体实现由 ContainerSandbox.RunScript 提供
								if err := m.hookRunner.RunScript(ctx, hookPath, inst.installPath); err != nil {
									slog.Warn("marketplace: uninstall hook failed", "ext", inst.id, "err", err)
								}
							} else {
								// hookRunner 未注入：skip，记录日志提示调用方配置 ContainerSandbox
								slog.Warn("marketplace: uninstall hook skipped (no HookRunner injected, call WithHookRunner to enable)",
									"ext", inst.id, "hook", hookPath)
							}
						}
					}
				}
			}

			_ = os.RemoveAll(inst.installPath)
		}

		_, _ = m.db.ExecContext(ctx, "DELETE FROM extension_instances WHERE id=?", inst.id)

		m.cleanCatalog(ctx, inst.origin, catalogID)
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
		_, _ = m.db.ExecContext(ctx, "DELETE FROM mcp_servers WHERE id=?", runtimeID)
	case "skill":
		// 独立安装的 skill：硬删除（非插件来源，卸载即消失）
		if runtimeID != "" {
			_, _ = m.db.ExecContext(ctx, "DELETE FROM skills WHERE name=?", runtimeID)
		}
	case "plugin":
		if runtimeID == "" {
			break
		}
		remover, _ := m.mcpMgr.(mcpRemover)
		m.removePluginRuntime(ctx, runtimeID, remover)
	case "app":
		if runtimeID != "" {
			_, _ = m.db.ExecContext(ctx, "DELETE FROM apps WHERE id=?", runtimeID)
		}
	}
}

func (m *Manager) removePluginRuntime(ctx context.Context, runtimeID string, remover interface{ Remove(id string) }) {
	// 从 mcp_servers 表读取所有子 MCP ID，停止运行时连接
	if remover != nil {
		mcpRows, err := m.db.QueryContext(ctx, "SELECT id FROM mcp_servers WHERE plugin_id=?", runtimeID)
		if err == nil {
			for mcpRows.Next() {
				var serverID string
				if mcpRows.Scan(&serverID) == nil {
					remover.Remove(serverID)
				}
			}
			mcpRows.Close()
		}
	}
	// 硬删除子 MCP（卸载即消失，不留 deprecated 脏数据）
	_, _ = m.db.ExecContext(ctx, "DELETE FROM mcp_servers WHERE plugin_id=?", runtimeID)
	// 硬删除子 skills（同上）
	_, _ = m.db.ExecContext(ctx, "DELETE FROM skills WHERE plugin_id=?", runtimeID)
	_, _ = m.db.ExecContext(ctx, "DELETE FROM plugins WHERE id=?", runtimeID)
}

func (m *Manager) cleanCatalog(ctx context.Context, origin, catalogID string) {
	if origin == "user" {
		_, _ = m.db.ExecContext(ctx, "DELETE FROM extension_catalog WHERE id=?", catalogID)
	} else if origin == "marketplace" {
		var isBuiltin int
		err := m.db.QueryRowContext(ctx, "SELECT is_builtin FROM plugin_marketplaces WHERE id = (SELECT marketplace_id FROM extension_catalog WHERE id=?)", catalogID).Scan(&isBuiltin)
		if err == nil && isBuiltin == 0 {
			_, _ = m.db.ExecContext(ctx, "DELETE FROM extension_catalog WHERE id=?", catalogID)
		}
	}
}

// UpdateStatus 更新 extension_instances 状态
func (m *Manager) UpdateStatus(ctx context.Context, id, status string) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE extension_instances SET status=?, updated_at=? WHERE id=?`,
		status, time.Now().UTC().Format(time.RFC3339), id)
	return err
}
