// Package sandbox — ExecEnvelope：统一执行信封，所有外部代码执行的单一权威入口。
package sandbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

type ExecKind string

const (
	KindToolExecute     ExecKind = "tool_execute"
	KindProcessSpawn    ExecKind = "process_spawn"
	KindScriptExecute   ExecKind = "script_execute"
	KindHookExecute     ExecKind = "hook_execute"
	KindBrowserAutomate ExecKind = "browser_automate"
)

const (
	PrincipalAgent  = "agent"
	PrincipalMCPMgr = "mcp_mgr"
)

// ExecRequest 一次执行请求的完整上下文。调用方负责填充 TrustTier（来自 DB 中对应扩展记录）。
type ExecRequest struct {
	Principal string
	Kind      ExecKind
	Resource  string
	TrustTier types.TrustTier

	Tool types.Tool // Kind=Tool/Script 时填充（Source/Capability/SideEffects 驱动 tier）

	Input       []byte
	ScriptPath  string
	ScriptBytes []byte
	Command     string   // 任意 shell 命令字符串（bash -c 语义），当前仅 Hook 引擎使用，与 ScriptPath 互斥
	ExtraEnv    []string // 追加环境变量，随 SandboxSpec 透传给脚本执行路径（当前仅 Hook 引擎使用）

	TaintLevel types.TaintLevel
	CapToken   *token.Token

	CPUQuotaMs int
	IOBudget   int64

	AllowNet bool
	DryRun   bool
}

type ExecResult struct {
	Success     bool
	Output      []byte
	Error       string
	LatencyMs   int64
	TaintLevel  types.TaintLevel
	SandboxTier types.SandboxTier
	ImageParts  []types.ImagePart
}

// HookFirer 供 ExecEnvelope 在工具调用前后触发匹配的 PreToolUse/PostToolUse Hook。
// 实现见 internal/action/hook.Runner；consumer-side 接口定义于此打破依赖环
// （internal/action/hook 已反向依赖 internal/sandbox 拿 CmdRunner/ExecEnvelope 类型）。
// Boot 通过 SetHookFirer 注入，nil 时 Execute 跳过 Hook 触发（不阻断主流程）。
type HookFirer interface {
	// FirePreToolUse 触发 PreToolUse，可否决本次工具调用（不能推翻 PolicyGate 已给出的 allow，
	// 只能在 allow 基础上追加拒绝——veto-only，不构成第二策略引擎）。
	FirePreToolUse(ctx context.Context, toolName string, toolInput map[string]any, sessionID string) (blocked bool, reason string)
	// FirePostToolUse 触发 PostToolUse，fire-and-forget，不影响已产出的 ExecResult。
	FirePostToolUse(ctx context.Context, toolName string, toolInput map[string]any, output string, sessionID string)
}

type ExecEnvelope struct {
	policy        protocol.PolicyGate
	router        *SandboxRouter
	hwTier        int
	goos          string
	tokenVerifier TokenVerifier // Boot 注入，打破依赖环
	hookFirer     HookFirer     // Boot 注入（可选），nil = 不触发 Hook
}

func NewExecEnvelope(policy protocol.PolicyGate, router *SandboxRouter, hwTier int, goos string, verifier TokenVerifier) *ExecEnvelope {
	return &ExecEnvelope{policy: policy, router: router, hwTier: hwTier, goos: goos, tokenVerifier: verifier}
}

// SetHookFirer 注入 PreToolUse/PostToolUse 触发器。未调用则 Execute 不触发 Hook。
// 独立 setter 而非构造参数：避免变更已有 ~20 处 NewExecEnvelope 调用点（含测试）签名。
func (e *ExecEnvelope) SetHookFirer(f HookFirer) {
	e.hookFirer = f
}

//nolint:gocyclo
func (e *ExecEnvelope) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	start := time.Now()

	// Step 1: PolicyGate（deny-by-default）
	if e.policy == nil {
		return nil, apperr.New(apperr.CodeForbidden, "exec_envelope: policy gate not initialized (deny-by-default)")
	}
	validToken := false
	if req.CapToken != nil && e.tokenVerifier != nil {
		validToken = e.tokenVerifier.Verify(req.CapToken) == nil
	}

	evalCtx := map[string]any{
		"trust_tier":             int(req.TrustTier),
		"risk_level":             int(req.Tool.RiskLevel),
		"tool_source":            string(req.Tool.Source),
		"kind":                   string(req.Kind),
		"allow_net":              req.AllowNet,
		"capability_token_valid": validToken,
	}
	allowed, pErr := e.policy.IsAuthorized(ctx, req.Principal, string(req.Kind), req.Resource, evalCtx)
	if pErr != nil || !allowed {
		reason := "policy denied"
		if pErr != nil {
			reason = pErr.Error()
		}
		slog.WarnContext(ctx, "exec_envelope: policy denied",
			"principal", req.Principal, "kind", req.Kind, "resource", req.Resource,
			"trust_tier", int(req.TrustTier), "reason", reason)
		return &ExecResult{Success: false, Error: "exec_envelope: " + reason,
			LatencyMs: time.Since(start).Milliseconds(), TaintLevel: req.TaintLevel}, nil
	}

	// Step 1.5: PreToolUse Hook（veto-only，不构成第二策略引擎——只能在 Step 1 已 allow
	// 的基础上追加拒绝，不能推翻 deny）。Kind=KindHookExecute 时跳过，防止 Hook 自身执行
	// 递归触发 PreToolUse（Hook 脚本执行本身也经本入口，见 Step 4 之后的说明）。
	if e.hookFirer != nil && req.Kind != KindHookExecute {
		if blocked, reason := e.hookFirer.FirePreToolUse(ctx, req.Resource, hookToolInput(req), ""); blocked {
			slog.WarnContext(ctx, "exec_envelope: blocked by PreToolUse hook",
				"principal", req.Principal, "kind", req.Kind, "resource", req.Resource, "reason", reason)
			return &ExecResult{Success: false, Error: "exec_envelope: pre_tool_use hook blocked: " + reason,
				LatencyMs: time.Since(start).Milliseconds(), TaintLevel: req.TaintLevel}, nil
		}
	}

	// Step 2: 沙箱等级（信任 + 工具属性 → tier）
	actualTier, tierErr := AssignSandboxTier(req.Tool, req.TrustTier, e.hwTier, e.goos)
	if tierErr != nil {
		return nil, apperr.Wrap(apperr.CodeSandboxTier0Limit, "exec_envelope: sandbox tier rejected", tierErr)
	}

	// Step 3: Capability Token（Privileged 强制；走 boot 注入的统一校验，语义同 tool.go）
	if req.Tool.Capability >= types.CapPrivileged {
		if req.CapToken == nil || e.tokenVerifier == nil || e.tokenVerifier.Verify(req.CapToken) != nil {
			return &ExecResult{Success: false, //nolint:nilerr
				Error:     "exec_envelope: privileged action requires valid capability token",
				LatencyMs: time.Since(start).Milliseconds(), TaintLevel: req.TaintLevel}, nil
		}
	}

	// Step 4: 路由（fail-closed：不可信代码所需隔离不可用时拒绝，不降级）
	provider, routeErr := e.router.RouteByTier(actualTier, req.TrustTier)
	if routeErr != nil {
		return nil, apperr.Wrap(apperr.CodeForbidden, "exec_envelope: route failed (isolation unavailable)", routeErr)
	}

	spec := SandboxSpec{
		ToolName:    req.Resource,
		Input:       req.Input,
		SandboxTier: actualTier,
		Capability:  req.Tool.Capability,
		SideEffects: req.Tool.SideEffects,
		ScriptPath:  req.ScriptPath,
		ScriptBytes: req.ScriptBytes,
		Command:     req.Command,
		ExtraEnv:    req.ExtraEnv,
		CPUQuotaMs:  req.CPUQuotaMs,
		IOBudget:    req.IOBudget,
		SystemTier:  e.hwTier,
		TaintLevel:  req.TaintLevel,
		DryRunMode:  req.DryRun,
	}
	toolResult, execErr := provider.Run(ctx, spec)
	if execErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "exec_envelope: execution failed", execErr)
	}

	// Step 5: TaintLevel only-up（max 传播；Community 及以下输出至少 TaintHigh）
	outTaint := req.TaintLevel
	if toolResult.TaintLevel > outTaint {
		outTaint = toolResult.TaintLevel
	}
	if req.TrustTier <= types.TrustCommunity && outTaint < types.TaintHigh {
		outTaint = types.TaintHigh
	}

	slog.InfoContext(ctx, "exec_envelope: executed",
		"kind", req.Kind, "resource", req.Resource, "trust_tier", int(req.TrustTier),
		"actual_tier", int(actualTier), "taint", int(outTaint), "latency_ms", time.Since(start).Milliseconds())

	// PostToolUse Hook：fire-and-forget，不得回写 ExecResult（HE-Rule-5，Hook 是协处理器，
	// 不得反向操控主流程）；异步执行避免给工具调用返回路径叠加 Hook 延迟。
	if e.hookFirer != nil && req.Kind != KindHookExecute {
		firer, resource, toolIn, out := e.hookFirer, req.Resource, hookToolInput(req), string(toolResult.Output)
		concurrent.SafeGo(context.WithoutCancel(ctx), "envelope.fire_post_tool_use", func(ctx context.Context) {
			firer.FirePostToolUse(ctx, resource, toolIn, out, "")
		})
	}

	return &ExecResult{
		Success: toolResult.Success, Output: toolResult.Output, Error: toolResult.Error,
		LatencyMs: time.Since(start).Milliseconds(), TaintLevel: outTaint,
		SandboxTier: actualTier, ImageParts: toolResult.ImageParts,
	}, nil
}

// hookToolInput 把 ExecRequest 归一为 Hook 引擎需要的 map[string]any 视图。
// ExecRequest 本身无结构化 tool_input，Input 为已编码字节流，原样转字符串传递。
func hookToolInput(req ExecRequest) map[string]any {
	m := map[string]any{"kind": string(req.Kind)}
	if len(req.Input) > 0 {
		m["input"] = string(req.Input)
	}
	if req.ScriptPath != "" {
		m["script_path"] = req.ScriptPath
	}
	return m
}
