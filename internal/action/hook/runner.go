package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const defaultTimeout = 30 * time.Second

// Runner 并发执行匹配事件的所有 Hook 处理器。
// 输出通过 Results() 返回，调用方负责 TaintLevel=High 封装。
type Runner struct {
	registry *Registry
}

func NewRunner(registry *Registry) *Runner {
	return &Runner{registry: registry}
}

// Fire 触发指定事件，并发执行所有匹配的 handler。
// 返回所有结果；任一失败不中断其他（可观测但不阻断主流程）。
func (r *Runner) Fire(ctx context.Context, input HookInput) []HookResult {
	groups := r.registry.Match(input.Event, input.ToolName)
	if len(groups) == 0 {
		return nil
	}

	type indexed struct {
		idx int
		res HookResult
	}
	results := make([]HookResult, 0)
	ch := make(chan indexed, 16)
	var wg sync.WaitGroup

	idx := 0
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Type != "command" {
				continue
			}
			wg.Add(1)
			go func(i int, handler HandlerConfig) {
				defer wg.Done()
				ch <- indexed{i, runCommand(ctx, handler, input)}
			}(idx, h)
			idx++
		}
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	for item := range ch {
		results = append(results, item.res)
	}
	return results
}

func runCommand(ctx context.Context, cfg HandlerConfig, input HookInput) HookResult {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	payload, err := json.Marshal(input)
	if err != nil {
		return HookResult{
			Event:   input.Event,
			Handler: cfg.Command,
			Err:     apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("hook: marshal input: %v", err), err),
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// shell 执行：sh -c <command>，stdin 传入 JSON payload
	cmd := exec.CommandContext(runCtx, "sh", "-c", cfg.Command)
	cmd.Stdin = bytes.NewReader(payload)
	// 最小环境变量隔离：仅保留基本 PATH，防止 hook 脏读宿主进程敖感环境变量
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin"}
	// Linux: 注入 namespace 隔离（与 ContainerSandbox.RunScript 一致）
	if attrs := hookSysProcAttr(); attrs != nil {
		cmd.SysProcAttr = attrs
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start).Milliseconds()

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return HookResult{
		Event:      input.Event,
		Handler:    cfg.Command,
		ExitCode:   exitCode,
		Stdout:     strings.TrimSpace(stdout.String()),
		Stderr:     strings.TrimSpace(stderr.String()),
		DurationMs: dur,
		Err:        runErr,
	}
}

// compileMatchers 编译 MatcherGroup 列表的正则。
func compileMatchers(groups []MatcherGroup) []MatcherGroup {
	out := make([]MatcherGroup, len(groups))
	for i, g := range groups {
		out[i] = g
		if g.Matcher != "" {
			re, err := regexp.Compile(g.Matcher)
			if err == nil {
				out[i].compiled = re
			}
		}
	}
	return out
}
