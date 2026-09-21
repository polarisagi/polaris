// 运行时状态发布：单实例锁 + run/ 三文件（端口/令牌/PID）。
//
// 客户端（CLI、桌面外壳）靠这组文件发现本机守护进程并取得调用凭证
// （ADR: docs/arch/decisions/ADR-0096-desktop-shell-and-daemon-client-split.md 决策五/六/七）。
// 锁在 boot 早期取得——晚于它的每一步都在改写共享数据目录，两个实例并发走到那里
// 会互相覆盖数据库与配置，而现象只是零星的数据错乱，不会指向"跑了两份"。
package main

import (
	"os"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/runtimeinfo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 单实例锁的获取重试参数。总窗口约 1 秒：远大于 probeInstanceLock 的持锁时长
// （微秒级），又远小于人能察觉的启动延迟。
const (
	lockRetries       = 10
	lockRetryInterval = 100 * time.Millisecond
)

// runtimeHandle 持有本进程的单实例锁与运行时文件路径。
type runtimeHandle struct {
	lock  *os.File
	paths runtimeinfo.Paths
	// Token 是本次启动的本地令牌，随 publish 落盘，同时供网关鉴权比对。
	Token string
}

// acquireRuntime 取得单实例锁并生成本次启动的本地令牌。
// 端口此时尚未确定（可能配置为 0 由内核分配），故 run/ 文件在 publish 时才写。
func acquireRuntime(layout config.DataLayout) (*runtimeHandle, error) {
	lock, err := lockInstance(layout.RunLock)
	if err != nil {
		return nil, err
	}
	token, err := runtimeinfo.NewToken()
	if err != nil {
		unlockInstance(lock)
		return nil, apperr.Wrap(apperr.CodeInternal, "runtime: 生成本地令牌失败", err)
	}
	return &runtimeHandle{
		lock: lock,
		paths: runtimeinfo.Paths{
			PID:   layout.RunPID,
			Port:  layout.RunPort,
			Token: layout.RunToken,
		},
		Token: token,
	}, nil
}

// publish 在 HTTP 服务实际监听成功后写入端口与令牌。
// 传入的是**实际绑定端口**而非配置值：配置为 0 时二者不同，写配置值会让所有
// 客户端连向 0 号端口。
func (h *runtimeHandle) publish(port int) error {
	if h == nil {
		return apperr.New(apperr.CodeInternal, "runtime: handle 为空")
	}
	if err := runtimeinfo.Write(h.paths, runtimeinfo.State{
		PID:   os.Getpid(),
		Port:  port,
		Token: h.Token,
	}); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "runtime: 发布运行时状态失败", err)
	}
	return nil
}

// release 清理运行时文件并释放锁。幂等，供 defer 调用。
func (h *runtimeHandle) release() {
	if h == nil {
		return
	}
	runtimeinfo.Remove(h.paths)
	unlockInstance(h.lock)
	h.lock = nil
}
