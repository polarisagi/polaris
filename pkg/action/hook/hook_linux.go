//go:build linux

package hook

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// hookSysProcAttr 返回 Linux 专属的 Hook 子进程安全属性（PID + 挂载 namespace 隔离）。
// 与 ContainerSandbox.RunScript 保持一致的隔离策略。
func hookSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS,
		Pdeathsig:  syscall.SIGKILL,
	}
}
