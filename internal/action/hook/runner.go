package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/classifier"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const defaultTimeout = 30 * time.Second

// Runner 并发执行匹配事件的所有 Hook 处理器。
// 输出通过 Results() 返回，调用方负责 TaintLevel=High 封装。
type Runner struct {
	registry *Registry
	policy   protocol.PolicyGate // nil → deny-by-default
}

func NewRunner(registry *Registry, policy protocol.PolicyGate) *Runner {
	return &Runner{registry: registry, policy: policy}
}

// Fire 触发指定事件，并发执行所有匹配的 handler。
// 返回所有结果；任一失败不中断其他（可观测但不阻断主流程）。
func (r *Runner) Fire(ctx context.Context, input HookInput) []HookResult {
	groups := r.registry.Match(input.Event, input.ToolName)
	if len(groups) == 0 {
		return nil
	}

	if r.policy != nil {
		allowed, pErr := r.policy.IsAuthorized(ctx, "agent", "hook_execute", string(input.Event),
			map[string]any{"event": string(input.Event), "tool_name": input.ToolName})
		if pErr != nil || !allowed {
			reason := "policy denied"
			if pErr != nil {
				reason = pErr.Error()
			}
			return []HookResult{{Event: input.Event, Err: apperr.New(apperr.CodeForbidden, "hook_execute denied: "+reason)}}
		}
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

	// ── 安全审核：CommandRiskClassifier ─────────────────────────────────────
	// Hook 命令与 bash/run_command 工具走相同风险分级：DENY→拒绝，HITL/WARN→审计日志。
	// 事件 hook 是用户配置，但仍需防御 fork bomb / wipe 等破坏性命令。
	verdict := classifier.Default().Classify(cfg.Command)
	switch verdict.Level {
	case classifier.RiskDeny:
		slog.Error("hook: command DENIED by risk classifier",
			"event", input.Event, "cmd", cfg.Command, "reason", verdict.Reason)
		return HookResult{
			Event:   input.Event,
			Handler: cfg.Command,
			Err:     apperr.New(apperr.CodeForbidden, fmt.Sprintf("hook: command denied: %s", verdict.Reason)),
		}
	case classifier.RiskHITL:
		// Phase1: 警告日志 + 继续执行（Phase2 将挂起等待 HITL 审批）
		slog.Warn("hook: command requires human approval (HITL) — executing in Phase1 mode",
			"event", input.Event, "cmd", cfg.Command, "reason", verdict.Reason)
	case classifier.RiskWarn:
		slog.Warn("hook: elevated-risk command executing",
			"event", input.Event, "cmd", cfg.Command, "reason", verdict.Reason)
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

	// shell 执行：sh -c <command>，stdin 传入 JSON payload。
	// 安全策略：最小 env（PATH 白名单）防止 hook 读宿主进程凭据（R1.15）。
	// 进程隔离由 PolicyGate（授权）+ CommandRiskClassifier（命令风险分级）双重保障。
	// Rust sandbox（bwrap/Seatbelt）未在此处启用——事件 hook 需访问灵活的文件路径；
	// bash/run_command 工具通过 WrapBashCmd 统一走 Rust 沙箱。
	cmd := exec.CommandContext(runCtx, "sh", "-c", cfg.Command)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}

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
