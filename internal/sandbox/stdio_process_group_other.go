//go:build !unix

package sandbox

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func setStdioProcessGroup(attr *syscall.SysProcAttr) *syscall.SysProcAttr { return attr }

func killStdioProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := cmd.Process.Kill()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stdio process: kill process", err)
	}
	return nil
}
