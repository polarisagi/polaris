//go:build !linux

package action

import "syscall"

// ContainerSandboxSysProcAttr 非 Linux 平台不支持 namespace 隔离，返回 nil。
func ContainerSandboxSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// containerSandboxSysProcAttr 内部别名。
func containerSandboxSysProcAttr() *syscall.SysProcAttr {
	return nil
}
