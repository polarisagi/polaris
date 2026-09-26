package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type ExtensionUninstallPayload struct {
	InstanceID  string `json:"instance_id"`
	CatalogID   string `json:"catalog_id"`
	InstallPath string `json:"install_path"`
	ExtType     string `json:"ext_type"`
	TrustTier   int    `json:"trust_tier"`
	RuntimeID   string `json:"runtime_id"`
}

// RuntimeUninstaller 按扩展类型清理运行时（停 MCP 连接、删 plugins/skills/mcp_servers 行、
// 删插件数据目录）。consumer-side 定义，实现为 extension/lifecycle.InstallFSM——sandbox（L1）
// 不得反向依赖 extension（L2）。
type RuntimeUninstaller interface {
	UninstallRuntime(ctx context.Context, extType, instanceID, runtimeID string) error
}

type ExtensionUninstallHandler struct {
	runtime RuntimeUninstaller
	extRepo protocol.ExtensionRepository
	timeout time.Duration // HE-6: 由 state.yaml M7Tool.ExtUninstallHookTimeoutS 注入，禁止硬编码
}

// NewExtensionUninstallHandler 构造卸载处理器。
// timeoutSeconds<=0 时兜底为 180s（与 config.DefaultThresholds() 默认值一致），
// 防止调用方未注入配置时退化为无超时挂起。
func NewExtensionUninstallHandler(runtime RuntimeUninstaller, extRepo protocol.ExtensionRepository, timeoutSeconds int) *ExtensionUninstallHandler {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 180
	}
	return &ExtensionUninstallHandler{
		runtime: runtime,
		extRepo: extRepo,
		timeout: time.Duration(timeoutSeconds) * time.Second,
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

	execCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	// 先清运行时再删文件：此前只删文件与实例记录，mcp_servers / skills / plugins 行与
	// 运行中的 MCP 进程全部残留（重启后按残留行重新拉起已"卸载"的扩展）。
	// 插件私有 install/uninstall 钩子已随 ADR-0103 删除；两家标准的 SessionEnd 等
	// 生命周期 hooks 由 hook 引擎按事件执行，不在卸载时运行。
	success := true
	if h.runtime == nil {
		slog.Error("extension_uninstall: runtime uninstaller not injected, runtime rows kept", "instance_id", payload.InstanceID)
		success = false
	} else if err := h.runtime.UninstallRuntime(execCtx, payload.ExtType, payload.InstanceID, payload.RuntimeID); err != nil {
		slog.Error("extension_uninstall: runtime cleanup failed", "instance_id", payload.InstanceID, "err", err)
		success = false
	}

	// 运行时清理失败则保留现场（文件与实例），仅成功时擦除。
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
	} else if err := h.extRepo.UpdateInstanceStatus(ctx, payload.InstanceID, "error", "runtime cleanup failed or timed out"); err != nil {
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
