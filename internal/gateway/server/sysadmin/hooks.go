package sysadmin

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/sysmgr/osutils"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// HookRunner 执行 $POLARIS_DATA_DIR/hooks/ 下的 Shell Script Hooks。
//
// 设计原则：类 git-hooks 模型，零依赖，脚本可用任意语言编写。
// 规范定义：docs/arch/00-Global-Dictionary.md §1 [ShellHooks]
//
// 事件点：
//   - gateway.startup      服务完全启动后（fire-and-forget）
//   - session.new          新会话创建时（fire-and-forget）
//   - message.before       处理用户消息前（同步，非零退出=拦截）
//   - message.after        AI 回复发出后（fire-and-forget）
//   - session.compact.before  上下文压缩开始前（同步）
//   - session.compact.after   上下文压缩完成后（fire-and-forget）
type HookRunner struct {
	dir string
}

// NewHookRunner 构造 HookRunner。
// 目录优先级：POLARIS_HOOKS_DIR > dataDir/hooks
func NewHookRunner(dataDir string) *HookRunner {
	dir := os.Getenv("POLARIS_HOOKS_DIR")
	if dir == "" {
		dir = filepath.Join(dataDir, "hooks")
	}
	return &HookRunner{dir: dir}
}

// Fire 异步触发非阻塞 hook（fire-and-forget）。
// 脚本不存在静默跳过；失败只记日志，不影响主流程。
func (h *HookRunner) Fire(event string, env map[string]string) {
	concurrent.SafeGo(context.Background(), "gateway.sysadmin.hook_fire", func(context.Context) {
		if _, _, err := h.exec(event, env, 5*time.Second); err != nil && !errors.Is(err, hookNotFoundError{}) {
			slog.Warn("hook: exec failed", "event", event, "err", err)
		}
	})
}

// FireBefore 同步触发 before hook，超时 2s。
// 返回 blocked=true 时主流程应拦截，reason 为脚本 stdout（作为拦截原因展示给用户）。
func (h *HookRunner) FireBefore(event string, env map[string]string) (blocked bool, reason string) {
	code, stdout, err := h.exec(event, env, 2*time.Second)
	if errors.Is(err, hookNotFoundError{}) {
		return false, ""
	}
	if err != nil {
		slog.Warn("hook: before exec error", "event", event, "err", err)
		return false, ""
	}
	if code != 0 {
		r := strings.TrimSpace(stdout)
		if r == "" {
			r = "hook blocked message"
		}
		return true, r
	}
	return false, ""
}

// exec 执行单个 hook 脚本，返回退出码、stdout+stderr 合并输出、错误。
func (h *HookRunner) exec(event string, env map[string]string, timeout time.Duration) (exitCode int, output string, err error) {
	path := filepath.Join(h.dir, event)
	if _, statErr := os.Stat(path); statErr != nil {
		return 0, "", hookNotFoundError{}
	}

	ctx := context.Background()
	exitCode, output, runErr := osutils.RunScript(ctx, path, buildHookEnv(env), timeout)
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn("hook: timeout", "event", event, "timeout", timeout, "err", apperr.New(apperr.CodeInternal, "log event"))
		}
		return exitCode, output, runErr
	}
	return exitCode, output, nil
}

func buildHookEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

type hookNotFoundError struct{}

func (hookNotFoundError) Error() string { return "hook script not found" }
