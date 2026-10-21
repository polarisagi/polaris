package agent

import (
	"context"
)

// registerHandoffWatcher 登记本次委派 watcher 的取消回调（R2-04）。
// 若存在先前未退出的 watcher，先显式将其取消以防跨轮次/跨生命周期泄漏。
func (a *Agent) registerHandoffWatcher(cancel context.CancelFunc) {
	if a == nil || a.sCtx == nil {
		return
	}
	a.sCtx.Mu.Lock()
	old := a.sCtx.HandoffWatchCancel
	a.sCtx.HandoffWatchCancel = cancel
	a.sCtx.Mu.Unlock()
	if old != nil {
		old()
	}
}

// cancelHandoffWatcher 显式取消并清理当前挂载的委派 watcher（R2-04）。
func (a *Agent) cancelHandoffWatcher() {
	if a == nil || a.sCtx == nil {
		return
	}
	a.sCtx.Mu.Lock()
	cancel := a.sCtx.HandoffWatchCancel
	a.sCtx.HandoffWatchCancel = nil
	a.sCtx.Mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Close 释放 Agent 相关资源，确保所有后台 watcher 退出（R2-04）。
func (a *Agent) Close() {
	a.cancelHandoffWatcher()
	a.releaseAllPreSubs()
}
