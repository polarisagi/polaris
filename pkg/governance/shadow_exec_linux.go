//go:build linux

package governance

import (
	"os/exec"
	"syscall"
)

func isolateNetwork(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNET,
	}
}
