package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type ExtensionUninstallPayload struct {
	InstanceID  string `json:"instance_id"`
	CatalogID   string `json:"catalog_id"`
	InstallPath string `json:"install_path"`
	ExtType     string `json:"ext_type"`
	TrustTier   int    `json:"trust_tier"`
	RuntimeID   string `json:"runtime_id"`
}

type ExtensionUninstallHandler struct {
	router      *SandboxRouter
	extRepo     protocol.ExtensionRepository
	hookTimeout time.Duration // HE-6: 由 state.yaml M7Tool.ExtUninstallHookTimeoutS 注入，禁止硬编码
}

// NewExtensionUninstallHandler 构造卸载 Hook 处理器。
// hookTimeoutSeconds<=0 时兜底为 180s（与 config.DefaultThresholds() 默认值一致），
// 防止调用方未注入配置时退化为无超时挂起。
func NewExtensionUninstallHandler(router *SandboxRouter, extRepo protocol.ExtensionRepository, hookTimeoutSeconds int) *ExtensionUninstallHandler {
	if hookTimeoutSeconds <= 0 {
		hookTimeoutSeconds = 180
	}
	return &ExtensionUninstallHandler{
		router:      router,
		extRepo:     extRepo,
		hookTimeout: time.Duration(hookTimeoutSeconds) * time.Second,
	}
}

// Handle consumes the extension_uninstall event.
//nolint:nestif

func (h *ExtensionUninstallHandler) Handle(ctx context.Context, record *store.OutboxRecord) error {
	var payload ExtensionUninstallPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		slog.Error("sandbox: failed to unmarshal extension_uninstall payload", "err", err)
		return nil // Drop invalid payload
	}

	// Create a context with timeout to force destroy if hanging
	execCtx, cancel := context.WithTimeout(ctx, h.hookTimeout)
	defer cancel()

	success := true

	if payload.InstallPath != "" && payload.ExtType == "plugin" {
		success = h.executeHook(execCtx, payload)
	}

	// Timeout 则强制保留现场，仅成功时擦除文件。
	//
	// 三处错误此前全被 `_ =` 吞掉，后果各不相同且都不可见：
	//   - RemoveAll 失败 → 磁盘残留扩展文件，重装时可能撞上旧文件
	//   - DeleteInstance 失败 → DB 里仍有该实例记录，但文件已删，UI 显示一个
	//     点开就报错的"幽灵扩展"（文件与 DB 反向不一致）
	//   - UpdateInstanceStatus 失败 → 卸载失败这件事本身没记下来，实例卡在
	//     旧状态，运维看不出它需要人工介入
	// 卸载是尽力而为的清理流程，单步失败不回滚（回滚需要把已删的文件变回来，
	// 做不到），故记录后继续；错误向上聚合返回，让调用方能感知未清理干净。
	var errs []error
	if success {
		errs = append(errs, removeInstallPath(payload)...)
		if err := h.extRepo.DeleteInstance(ctx, payload.InstanceID); err != nil {
			slog.Error("extension_uninstall: 实例记录删除失败，文件已删但 DB 仍有记录（幽灵扩展）",
				"instance_id", payload.InstanceID, "err", err)
			errs = append(errs, apperr.Wrap(apperr.CodeInternal, "extension_uninstall: delete instance", err))
		}
	} else if err := h.extRepo.UpdateInstanceStatus(ctx, payload.InstanceID, "error", "uninstall hook failed or timed out"); err != nil {
		slog.Error("extension_uninstall: 失败状态回写失败，实例将停留在旧状态且无人工介入线索",
			"instance_id", payload.InstanceID, "err", err)
		errs = append(errs, apperr.Wrap(apperr.CodeInternal, "extension_uninstall: mark instance error", err))
	}

	if len(errs) > 0 {
		return apperr.Wrap(apperr.CodeInternal, "ExtensionUninstallHandler: 卸载清理未完全成功", errors.Join(errs...))
	}
	return nil
}

// removeInstallPath 擦除扩展安装目录，失败只记录不中断（见调用点注释）。
// 单独成函数是 nestif 治理，行为与内联时一致。
func removeInstallPath(payload ExtensionUninstallPayload) []error {
	if payload.InstallPath == "" {
		return nil
	}
	if err := os.RemoveAll(payload.InstallPath); err != nil {
		slog.Error("extension_uninstall: 扩展文件删除失败，磁盘可能残留",
			"instance_id", payload.InstanceID, "path", payload.InstallPath, "err", err)
		return []error{apperr.Wrap(apperr.CodeInternal, "extension_uninstall: remove install path", err)}
	}
	return nil
}

func (h *ExtensionUninstallHandler) executeHook(ctx context.Context, payload ExtensionUninstallPayload) bool {
	raw, err := os.ReadFile(filepath.Join(payload.InstallPath, "plugin.json"))
	if err != nil {
		return true // No plugin.json, nothing to do
	}

	var bundle protocol.PluginBundleManifest
	_ = json.Unmarshal(raw, &bundle)

	hook := bundle.Hooks["uninstall"]
	if hook == "" {
		return true // No uninstall hook
	}

	hookPath := filepath.Join(payload.InstallPath, hook)
	cleanHook := filepath.Clean(hookPath)
	cleanBase := filepath.Clean(payload.InstallPath)
	if !strings.HasPrefix(cleanHook, cleanBase+string(filepath.Separator)) && cleanHook != cleanBase {
		return true // Invalid path traversal
	}

	provider, err := h.router.RouteByTier(types.SandboxContainer, types.TrustTier(payload.TrustTier))
	if err != nil {
		slog.Warn("sandbox: failed to route uninstall hook", "err", err, "ext", payload.InstanceID)
		return false
	}

	runner, ok := provider.(interface {
		RunHook(context.Context, string, string) error
	})
	if !ok {
		slog.Warn("sandbox: routed provider does not support RunHook", "ext", payload.InstanceID)
		return true // Ignore if unsupported
	}

	if err := runner.RunHook(ctx, hookPath, payload.InstallPath); err != nil {
		slog.Warn("sandbox: uninstall hook failed", "err", err, "ext", payload.InstanceID)
		return false
	}

	return true
}
