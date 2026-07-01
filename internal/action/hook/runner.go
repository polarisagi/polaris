package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/classifier"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const defaultTimeout = 30 * time.Second

// Runner 并发执行匹配事件的所有 Hook 处理器。
// 输出通过 Results() 返回，调用方负责 TaintLevel=High 封装。
type Runner struct {
	registry  *Registry
	policy    protocol.PolicyGate // nil → deny-by-default
	cmdRunner sandbox.CmdRunner   // nil → fail-closed，不裸跑（HE-Rule 2）
}

// NewRunner 构造 Runner。cmdRunner 复用 internal/sandbox.CmdRunner（Rust bwrap/
// Seatbelt 统一沙箱），与 ContainerSandbox.RunHook 走同一物理隔离实现——此前
// 这里是独立的 exec.CommandContext(sh, -c, ...) 裸执行，只靠 PolicyGate 授权 +
// CommandRiskClassifier 正则分级兜底，是概率过滤当安全边界，违反 HE-Rule 2。
func NewRunner(registry *Registry, policy protocol.PolicyGate, cmdRunner sandbox.CmdRunner) *Runner {
	return &Runner{registry: registry, policy: policy, cmdRunner: cmdRunner}
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
				ch <- indexed{i, runCommand(ctx, r.cmdRunner, handler, input)}
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

// runCommand 执行单个 command 类型 Hook 处理器。
//
// 安全模型（两层，物理隔离为主，分级为辅——HE-Rule 2：不得只靠概率过滤当边界）：
//  1. CommandRiskClassifier 正则风险分级：DENY 直接拒绝；HITL/WARN 仅告警审计。
//  2. cmdRunner（Rust bwrap/Seatbelt 统一沙箱，同 ContainerSandbox.RunHook）：
//     真正的进程/网络隔离由这一层提供，不再是"裸 sh -c + 事后正则过滤"。
//
// cmdRunner 为 nil 时 fail-closed 直接拒绝，不回退裸执行——与本包其余 deny-by-default
// 风格（PolicyGate 缺失即拒绝）保持一致。
func runCommand(ctx context.Context, cmdRunner sandbox.CmdRunner, cfg HandlerConfig, input HookInput) HookResult {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	// ── 安全审核：CommandRiskClassifier（分级，非唯一边界）───────────────────
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

	if cmdRunner == nil {
		return HookResult{
			Event:   input.Event,
			Handler: cfg.Command,
			Err:     apperr.New(apperr.CodeForbidden, "hook: cmdRunner not configured, refusing to execute (fail-closed)"),
		}
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

	// stdin JSON payload 通过环境变量传递（HOOK_INPUT_JSON），因为 CmdRunner 的
	// shell 执行接口（bash -c）不提供 stdin 管道；hook 脚本按需自行读取该变量。
	// 最小 env（PATH 白名单 + payload）防止 hook 读宿主进程凭据（R1.15）。
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOOK_INPUT_JSON=" + string(payload),
	}

	start := time.Now()
	out, exitCode, method, runErr := cmdRunner.RunCmd(runCtx, sandbox.CmdRunnerCfg{
		Command:      cfg.Command,
		Env:          env,
		NetworkBlock: true, // 事件 hook 默认断网，对齐 CodeAct/技能脚本策略
		TimeoutMs:    uint64(timeout.Milliseconds()),
	})
	dur := time.Since(start).Milliseconds()

	if runErr != nil {
		return HookResult{
			Event:      input.Event,
			Handler:    cfg.Command,
			ExitCode:   -1,
			DurationMs: dur,
			Err:        apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("hook: sandboxed exec failed (method=%s)", method), runErr),
		}
	}

	return HookResult{
		Event:      input.Event,
		Handler:    cfg.Command,
		ExitCode:   exitCode,
		Stdout:     strings.TrimSpace(string(out)),
		DurationMs: dur,
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
