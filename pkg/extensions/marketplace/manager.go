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

type InstallRequest struct {
	Principal   string
	ExtensionID string
	ExtType     string // plugin, skill, mcp
	TrustTier   int
	Publisher   string
	HasHooks    bool
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

// InstallExtension handles the install flow with M11 Cedar-Gate.
func (m *Manager) InstallExtension(ctx context.Context, req InstallRequest) error {
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

	// PolicyGate 放行后写入 extension_instances，满足 ADR-0019 三层模型要求（P0-6）。
	// 原实现在此直接 return nil，不写 DB，违反 ADR-0019 Layer-1 SSoT 约束。
	instID, err := genExtID()
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "marketplace.InstallExtension: gen id", err)
	}
	name := req.ExtensionID
	if req.Publisher != "" {
		name = fmt.Sprintf("%s/%s", req.Publisher, req.ExtensionID)
	}
	// 先标记 status='installing'，防止重复安装竞争（UNIQUE INDEX on catalog_id）。
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO extension_instances
			(id, ext_type, origin, catalog_id, name, publisher, trust_tier, status)
		VALUES (?, ?, 'marketplace', ?, ?, ?, ?, 'installing')`,
		instID, req.ExtType, req.ExtensionID, name, req.Publisher, req.TrustTier,
	)
	if err != nil {
		// UNIQUE 冲突 → 已安装，幂等返回成功
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil
		}
		return perrors.Wrap(perrors.CodeInternal, "marketplace.InstallExtension: insert instance", err)
	}

	// 更新为 status='installed'（当前 MVP：无实际文件下载；Tier1+ 由下载器回调更新）。
	// 此处直接置为 installed，保持与 builtin 扩展注册路径行为一致。
	_, err = m.db.ExecContext(ctx, `
		UPDATE extension_instances SET status='installed' WHERE id=?`, instID)
	if err != nil {
		slog.Warn("marketplace: failed to mark extension installed", "id", instID, "err", err)
		// 非致命错误：记录警告，状态留在 installing（后续 reconcile 可修正）
	}
	return nil
}

// genExtID 生成 extension_instances.id（格式："ext_{8字节hex}"）。
func genExtID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ext_" + hex.EncodeToString(b), nil
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
