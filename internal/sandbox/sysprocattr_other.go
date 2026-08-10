//go:build !linux

package sandbox

import "syscall"

func setPdeathsig(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	return attr
}
