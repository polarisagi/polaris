//go:build linux

package action

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// ContainerSandboxSysProcAttr 返回 Linux 专属的子进程安全属性：
//   - CLONE_NEWUSER: 独立用户命名空间，允许普通用户执行后续的命名空间隔离
//   - CLONE_NEWPID: 独立 PID 命名空间，防止子进程枚举/信号攻击宿主
//   - CLONE_NEWNS:  独立挂载命名空间，防止子进程污染全局 mount 表
//   - Pdeathsig:   父进程退出时 SIGKILL 子进程，消灭孤儿进程
func ContainerSandboxSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		Pdeathsig: syscall.SIGKILL,
	}
}

// containerSandboxSysProcAttr 内部别名，供包内 ContainerSandbox.Run/RunScript 调用。
func containerSandboxSysProcAttr() *syscall.SysProcAttr {
	return ContainerSandboxSysProcAttr()
}
