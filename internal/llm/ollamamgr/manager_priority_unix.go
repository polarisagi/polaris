//go:build !windows

package ollamamgr

import (
	"log/slog"
	"os/exec"
	"syscall"
)

func prepareCmdAttrs(cmd *exec.Cmd) {
	// Unix 默认无需特别设置 SysProcAttr
}

func setProcessLowPriority(cmd *exec.Cmd) {
	if cmd.Process != nil && cmd.Process.Pid > 0 {
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 10); err != nil {
			slog.Debug("ollamamgr: failed to set low priority on child process", "err", err)
		}
	}
}
