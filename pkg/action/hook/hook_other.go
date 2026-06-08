//go:build !linux

package hook

import "syscall"

// hookSysProcAttr 非 Linux 平台不支持 namespace 隔离，返回 nil。
func hookSysProcAttr() *syscall.SysProcAttr {
	return nil
}
