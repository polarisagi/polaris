//go:build unix

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func setStdioProcessGroup(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Setpgid = true
	return attr
}

func killStdioProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stdio process: kill process group", err)
	}
	return nil
}
