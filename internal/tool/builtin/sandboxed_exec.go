package builtin

import (
	"context"
	"os/exec"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// runSandboxedArgv 通过 Rust V2 沙箱以 argv 形式（不经 shell 解释）执行外部命令，
// 供 git/ffmpeg/edge-tts 等内置工具复用，避免各自裸调 exec.Command 绕过沙箱边界。
// 复用 execInSandbox 的沙箱开关/bwrapPath 判定逻辑（与 bash/run_command 工具一致）。
func runSandboxedArgv(ctx context.Context, callerType protocol.SandboxCallerType, execPath string, execArgs []string,
	workDir string, allowedPaths []string, netAllow bool, timeoutMs uint64, sandboxEnabled bool, bwrapPath string) ([]byte, error) {

	var outBytes []byte
	var execErr error

	if sandboxEnabled {
		netPolicy := protocol.NetPolicyDeny
		if netAllow {
			netPolicy = protocol.NetPolicyAllow
		}

		sandboxCtx := protocol.SandboxContext{
			CallerType:    callerType,
			ExecPath:      execPath,
			ExecArgs:      execArgs,
			Workdir:       workDir,
			AllowedPaths:  allowedPaths,
			NetworkPolicy: netPolicy,
			BwrapPath:     bwrapPath,
			TimeoutMs:     timeoutMs,
		}

		var setupErr error
		outBytes, execErr, _, setupErr = execInSandbox(ctx, sandboxCtx)
		if setupErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "runSandboxedArgv: sandbox wrap failed", setupErr)
		}
	} else {
		// 沙箱禁用：env 清理 + workDir（与 execWithoutSandbox 类似，但针对 argv）
		cmd := exec.CommandContext(ctx, execPath, execArgs...)
		cmd.Dir = workDir
		cmd.Env = baseEnv()
		outBytes, execErr = cmd.CombinedOutput()
	}

	return outBytes, execErr
}
