//go:build !linux

package sandbox

import (
	"log/slog"
	"syscall"
)

// ContainerSandboxSysProcAttr 非 Linux 平台不支持 namespace 隔离，返回 nil。
// 调用方须确认进程在 SafeDialer + local_only L2/L3 防护下运行，禁止裸执行高危命令。
func ContainerSandboxSysProcAttr() *syscall.SysProcAttr {
	slog.Warn("sandbox: namespace isolation unavailable on non-Linux platform; process runs without container isolation — ensure SafeDialer/local_only protections are active")
	return nil
}

// containerSandboxSysProcAttr 内部别名。
func containerSandboxSysProcAttr() *syscall.SysProcAttr {
	return ContainerSandboxSysProcAttr()
}
