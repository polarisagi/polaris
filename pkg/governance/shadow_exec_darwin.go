//go:build !linux

package governance

import (
	"os/exec"
)

func isolateNetwork(cmd *exec.Cmd) {
	cmd.Env = append(cmd.Env, "POLARIS_NETWORK_OFFLINE=1")
}
