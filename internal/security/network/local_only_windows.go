//go:build windows

package network

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func (s *OSNetworkSandbox) enableOS() error {
	exePath, err := os.Executable()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "local_only: failed to get executable path for windows sandbox", err)
	}

	// Remove existing rule if any
	_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name=Polaris_Local_Only_Sandbox").Run()

	// Add new outbound blocking rule for this executable
	cmd := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name=Polaris_Local_Only_Sandbox",
		"dir=out",
		"action=block",
		fmt.Sprintf("program=%s", exePath),
		"enable=yes",
		"profile=any")

	if err := cmd.Run(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "local_only: failed to set windows firewall rule (requires admin privileges)", err)
	}

	return nil
}
