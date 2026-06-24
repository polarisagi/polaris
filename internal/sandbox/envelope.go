// Package sandbox — ExecEnvelope：统一执行信封，所有外部代码执行的单一权威入口。
package sandbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
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

type ExecEnvelope struct {
	policy        protocol.PolicyGate
	router        *SandboxRouter
	hwTier        int
	goos          string
	tokenVerifier TokenVerifier // Boot 注入，打破依赖环
}

func NewExecEnvelope(policy protocol.PolicyGate, router *SandboxRouter, hwTier int, goos string, verifier TokenVerifier) *ExecEnvelope {
	return &ExecEnvelope{policy: policy, router: router, hwTier: hwTier, goos: goos, tokenVerifier: verifier}
}

func (e *ExecEnvelope) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	start := time.Now()

	// Step 1: PolicyGate（deny-by-default）
	if e.policy == nil {
		return nil, apperr.New(apperr.CodeForbidden, "exec_envelope: policy gate not initialized (deny-by-default)")
	}
	evalCtx := map[string]any{
		"trust_tier":  int(req.TrustTier),
		"risk_level":  int(req.Tool.RiskLevel),
		"tool_source": string(req.Tool.Source),
		"kind":        string(req.Kind),
		"allow_net":   req.AllowNet,
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

	return &ExecResult{
		Success: toolResult.Success, Output: toolResult.Output, Error: toolResult.Error,
		LatencyMs: time.Since(start).Milliseconds(), TaintLevel: outTaint,
		SandboxTier: actualTier, ImageParts: toolResult.ImageParts,
	}, nil
}
