package tool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RunHookScript 执行用户配置的 hook 脚本（合法的 exec 调用封装层）。
// 此函数属于 pkg/action 工具层，是 R1.13 要求的合法 exec 封装位置。
func RunHookScript(ctx context.Context, path string, env []string, timeout time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path)
	cmd.Env = append(os.Environ(), env...)

	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if runErr := cmd.Run(); runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 1, buf.String(), nil
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode(), buf.String(), nil
		}
		return 1, buf.String(), runErr
	}
	return 0, buf.String(), nil
}
