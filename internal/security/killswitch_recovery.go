package security

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// KillSwitch 的跨重启持久化与人工恢复路径（R7 拆分自 killswitch.go）。
//
// 这些函数共享一个不变量：.fullstop 标记文件是熔断状态跨进程重启的**唯一**
// 凭证。读写它的每一处失败都必须 fail-closed —— 标记丢失意味着重启后系统
// 以为从未熔断过、直接恢复服务，正是熔断要防的事（ADR-0009）。
//
// 状态机与触发路径见 killswitch.go。

// IsFullStopped 返回当前是否处于 FullStop 状态（持锁读）。
func (ks *KillSwitch) IsFullStopped() bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.state == types.KillFullStop
}

// OnRecovery 注册恢复回调
func (ks *KillSwitch) OnRecovery(cb func(ctx context.Context)) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.recoveryCallback = cb
}

// removeFullStopFile 删除全停文件。
func (ks *KillSwitch) removeFullStopFile() error {
	dataDir := ks.dataDir
	if dataDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dataDir = filepath.Join(home, ".polarisagi/polaris")
		}
	}
	if dataDir == "" {
		return nil
	}
	fullStopFile := filepath.Join(dataDir, ".fullstop")
	if err := os.Remove(fullStopFile); err != nil && !os.IsNotExist(err) {
		return apperr.Wrap(apperr.CodeInternal, "failed to remove fullstop file", err)
	}
	return nil
}

// ManualRecover 线程安全地手动触发恢复（解除封印）。
func (ks *KillSwitch) ManualRecover(ctx context.Context, actor, reason string) error {
	ks.mu.Lock()
	wasSealed := ks.state == types.KillFullStop

	if wasSealed {
		if err := ks.removeFullStopFile(); err != nil {
			ks.mu.Unlock()
			slog.Error("killswitch: failed to remove .fullstop file", "err", err)
			return apperr.Wrap(apperr.CodeInternal, "killswitch: failed to remove .fullstop file", err)
		}
	}

	ks.actor = actor
	ks.monitors.errorCounter = 0
	ks.monitors.safetyViolations = 0
	ks.monitors.fatalViolations = 0
	ks.monitors.irreversibleAttempts = 0
	ks.transitionLocked(types.KillNormal, reason)
	cb := ks.recoveryCallback
	ks.mu.Unlock()

	if wasSealed && cb != nil {
		cb(ctx)
	}
	return nil
}

// Unseal 是最高权限的管理端点调用的解封方法，等价于 ManualRecover
func (ks *KillSwitch) Unseal(ctx context.Context, actor, reason string) error {
	return ks.ManualRecover(ctx, actor, reason)
}
