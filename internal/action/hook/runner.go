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
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

const defaultTimeout = 30 * time.Second

// Runner 并发执行匹配事件的所有 Hook 处理器。
// 输出通过 Results() 返回，调用方负责 TaintLevel=High 封装。
//
// 同时实现 sandbox.HookFirer 接口（FirePreToolUse/FirePostToolUse），供
// ExecEnvelope 在工具调用前后触发本引擎——2026-07-02 起 Runner 自身的命令执行
// 也改经 ExecEnvelope.Execute（Kind=KindHookExecute），不再直接调用 CmdRunner，
// 使 Hook 执行获得与其他执行类型一致的沙箱分级/Capability Token/Taint 传播（见
// envelope.go Step 1.5/PostToolUse 触发点，ADR-0015 修订记录 2026-07-02）。
type Runner struct {
	registry       *Registry
	policy         protocol.PolicyGate    // nil → deny-by-default（事件级粗粒度检查）
	envelope       *sandbox.ExecEnvelope  // nil → fail-closed，不裸跑（HE-Rule 2）
	piiDetector    *guard.PIIDetector     // 序列化 HookInput 前脱敏 ToolInput/Output（防本地脚本读取真实 PII）
	piiDesens      *guard.PIIDesensitizer // PII 格式保留脱敏器
	riskClassifier *classifier.CommandRiskClassifier
}

// NewRunner 构造 Runner。envelope 复用 internal/sandbox.ExecEnvelope（统一执行信封，
// PolicyGate→沙箱分级→Capability Token→路由→执行→Taint only-up 五步），与 CodeAct/Skill/
// MCP 走同一入口——此前这里是独立的 CmdRunner 旁路，只做了一次事件级 PolicyGate 检查，
// 跳过了沙箱分级/Capability Token/Taint 传播三步，与 envelope.go"单一权威入口"的声明矛盾。
// piiDetector 为 nil 时自动使用 guard.NewPIIDetector()（Tier 0 纯正则，无外部依赖）。
func NewRunner(registry *Registry, policy protocol.PolicyGate, envelope *sandbox.ExecEnvelope, piiDetector *guard.PIIDetector, piiDesens *guard.PIIDesensitizer) *Runner {
	if piiDetector == nil {
		piiDetector = guard.NewPIIDetector()
	}
	return &Runner{
		registry:       registry,
		policy:         policy,
		envelope:       envelope,
		piiDetector:    piiDetector,
		piiDesens:      piiDesens,
		riskClassifier: classifier.NewDefaultClassifier(),
	}
}

// FirePreToolUse 实现 sandbox.HookFirer。veto-only：只能在 ExecEnvelope Step 1
// PolicyGate 已 allow 的基础上追加拒绝，不能推翻 deny，不构成第二策略引擎。
func (r *Runner) FirePreToolUse(ctx context.Context, toolName string, toolInput map[string]any, sessionID string) (blocked bool, reason string) {
	results := r.Fire(ctx, HookInput{Event: EventPreToolUse, ToolName: toolName, ToolInput: toolInput, SessionID: sessionID})
	for _, res := range results {
		if res.Err == nil && res.ExitCode == 0 {
			continue
		}
		// 拦截原因优先取脚本 stdout（用户可读），Err 只在 stdout 为空时兜底
		// （如风险分级 DENY / envelope fail-closed，这类场景本就没有脚本输出）。
		reason := strings.TrimSpace(res.Stdout)
		if reason == "" && res.Err != nil {
			reason = res.Err.Error()
		}
		if reason == "" {
			reason = fmt.Sprintf("hook %q exited %d", res.Handler, res.ExitCode)
		}
		return true, reason
	}
	return false, ""
}

// FirePostToolUse 实现 sandbox.HookFirer。fire-and-forget，结果仅记日志。
func (r *Runner) FirePostToolUse(ctx context.Context, toolName string, toolInput map[string]any, output string, sessionID string) {
	results := r.Fire(ctx, HookInput{Event: EventPostToolUse, ToolName: toolName, ToolInput: toolInput, Output: output, SessionID: sessionID})
	for _, res := range results {
		if res.Err != nil {
			slog.WarnContext(ctx, "hook: PostToolUse handler failed", "tool", toolName, "handler", res.Handler, "err", res.Err)
		}
	}
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
			//nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
			go func(i int, handler HandlerConfig) {
				defer wg.Done()
				ch <- indexed{i, r.runCommand(ctx, handler, input)}
			}(idx, h)
			idx++
		}
	}

	//nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
	go func() {
		wg.Wait()
		close(ch)
	}()

	for item := range ch {
		results = append(results, item.res)
	}
	return results
}

// redactHookInput 对 HookInput 序列化前做 PII 脱敏。
//
// 背景：ToolInput/Output 可能携带真实工具调用参数（文件路径、bash 命令、MCP 参数等），
// 其中可能包含邮箱/手机号/身份证等 PII。Hook 脚本是 End-User 本地配置，天然可信度低于
// 系统内部组件，即使已通过 NetworkBlock 挡住直接外传，脚本仍可将内容写入本地任意可写
// 文件——序列化前脱敏是纵深防御的最后一道，不因"已断网"而省略（HE-Rule 2）。
func (r *Runner) redactHookInput(ctx context.Context, input HookInput) ([]byte, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("hook: marshal input: %v", err), err)
	}
	redacted, n, err := r.piiDetector.RedactWithMode(ctx, string(raw), "replace", r.piiDesens, nil)
	if err != nil {
		// 脱敏失败按失败关闭处理：宁可拒绝，不裸传未脱敏内容（HE-Rule 2）。
		return nil, apperr.Wrap(apperr.CodeInternal, "hook: PII redact failed", err)
	}
	if n > 0 {
		slog.WarnContext(ctx, "hook: redacted PII from hook input", "event", input.Event, "matches", n)
	}
	return []byte(redacted), nil
}

// runCommand 执行单个 command 类型 Hook 处理器。
//
// 安全模型（三层，物理隔离为主，分级为辅——HE-Rule 2：不得只靠概率过滤当边界）：
//  1. CommandRiskClassifier 正则风险分级：DENY 直接拒绝；HITL/WARN 仅告警审计。
//  2. PII 脱敏：HookInput 序列化前经 PIIDetector.Redact。
//  3. ExecEnvelope.Execute（Kind=KindHookExecute）：沙箱分级 + Capability Token +
//     Rust bwrap/Seatbelt 统一沙箱物理隔离，与 bash 工具、CodeAct、Skill 同一入口。
//
// envelope 为 nil 时 fail-closed 直接拒绝，不回退裸执行——与本包其余 deny-by-default
// 风格（PolicyGate 缺失即拒绝）保持一致。
func (r *Runner) runCommand(ctx context.Context, cfg HandlerConfig, input HookInput) HookResult {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	// ── 安全审核：CommandRiskClassifier（分级，非唯一边界）───────────────────
	verdict := r.riskClassifier.Classify(cfg.Command)
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

	if r.envelope == nil {
		return HookResult{
			Event:   input.Event,
			Handler: cfg.Command,
			Err:     apperr.New(apperr.CodeForbidden, "hook: envelope not configured, refusing to execute (fail-closed)"),
		}
	}

	payload, err := r.redactHookInput(ctx, input)
	if err != nil {
		return HookResult{Event: input.Event, Handler: cfg.Command, Err: err}
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// stdin JSON payload 通过环境变量传递（HOOK_INPUT_JSON），因为底层 CmdRunner 的
	// shell 执行接口（bash -c）不提供 stdin 管道；hook 脚本按需自行读取该变量。
	extraEnv := []string{"HOOK_INPUT_JSON=" + string(payload)}

	start := time.Now()
	result, execErr := r.envelope.Execute(runCtx, sandbox.ExecRequest{
		Principal: sandbox.PrincipalAgent,
		Kind:      sandbox.KindHookExecute,
		Resource:  "hook:" + string(input.Event),
		TrustTier: types.TrustLocal, // End-User 本地配置的 hooks.yaml，信任级别同本地 Skill
		Tool: types.Tool{
			Name:        cfg.Command,
			Source:      types.ToolSkill,
			Capability:  types.CapWriteLocal,
			SideEffects: []types.SideEffect{types.SideProcessSpawn}, // 强制路由至 Container/NativeOS tier
		},
		Command:    cfg.Command,
		ExtraEnv:   extraEnv,
		TaintLevel: types.TaintHigh, // Hook 输出强制 TaintLevel=High（安全不变量，见 M07 §15）
		AllowNet:   false,           // 事件 hook 默认断网，对齐 CodeAct/技能脚本策略
	})
	dur := time.Since(start).Milliseconds()

	if execErr != nil {
		return HookResult{
			Event:      input.Event,
			Handler:    cfg.Command,
			ExitCode:   -1,
			DurationMs: dur,
			Err:        apperr.Wrap(apperr.CodeInternal, "hook: envelope exec failed", execErr),
		}
	}
	if !result.Success {
		return HookResult{
			Event:      input.Event,
			Handler:    cfg.Command,
			ExitCode:   1,
			Stdout:     strings.TrimSpace(string(result.Output)),
			DurationMs: dur,
			Err:        apperr.New(apperr.CodeInternal, fmt.Sprintf("hook: %s", result.Error)),
		}
	}

	return HookResult{
		Event:      input.Event,
		Handler:    cfg.Command,
		ExitCode:   0,
		Stdout:     strings.TrimSpace(string(result.Output)),
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
