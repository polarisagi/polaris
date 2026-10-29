//go:build windows

package bash

import (
	"os/exec"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func setProcessGroup(cmd *exec.Cmd) {
	// Windows 不支持 Setpgid
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "kill process", err)
	}
	return nil
}
