//go:build !windows

package bash

import (
	"os/exec"
	"syscall"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if pid > 0 {
		// 负 PID 杀掉整个进程组（R2-16）
		err := syscall.Kill(-pid, syscall.SIGKILL)
		if err == nil {
			return nil
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "kill process fallback", err)
	}
	return nil
}
