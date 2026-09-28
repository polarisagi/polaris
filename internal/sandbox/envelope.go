// Package sandbox — ExecEnvelope：统一执行信封，所有外部代码执行的单一权威入口。
package sandbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type ExecKind string

const (
	KindToolExecute     ExecKind = protocol.PolicyActionToolExecute
	KindProcessSpawn    ExecKind = "process_spawn"
	KindScriptExecute   ExecKind = "script_execute"
	KindBrowserAutomate ExecKind = "browser_automate"
)

const (
	PrincipalAgent  = protocol.PolicyPrincipalAgent
	PrincipalMCPMgr = "mcp_mgr"
	// PrincipalMCPApp MCP Apps View 经 /v1/mcp-apps/views/{id}/rpc 发起的工具调用
	// （HE-3：与模型发起的调用走同一 Execute 入口，靠 Principal 区分供 PolicyGate/
	// 审计判别来源，而非另开一条旁路）。
	PrincipalMCPApp = "mcp_app"
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
	ExtraEnv    []string // 追加环境变量，随 SandboxSpec 透传给脚本执行路径

	TaintLevel types.TaintLevel
	CapToken   *token.Token

	CPUQuotaMs int
	IOBudget   int64

	AllowNet bool
	DryRun   bool

	// SessionID/Language/StatefulSession D4/ADR-0008：CodeAct 长驻会话专用。
	// StatefulSession=true 且 SessionID 非空时，Execute() 会把 actualTier 强制
	// 覆盖为 types.SandboxPersistent（不经 AssignSandboxTier 的通用信任/能力
	// 判定——那套判定不了解"这是一个有状态会话"这一语义）。调用方（目前仅
	// code_act.go）必须在设置 StatefulSession=true 之前自行确认
	// PersistentSandboxAvailable()==true，并据此决定是否已经把原始代码
	// （而非 pickle 包装后的代码）写入 ScriptPath——本结构体不做这层判断。
	SessionID       string
	Language        string
	StatefulSession bool

	// AppViewID / AppSessionID：Principal=PrincipalMCPApp 时标注发起调用的 MCP Apps
	// View/会话，供审计日志与 PolicyGate evalCtx 区分"App 发起"与"模型发起"
	// （M8f-1，HE-3）。模型发起的调用两者均为空。
	AppViewID    string
	AppSessionID string
}

type ExecResult struct {
	Success     bool
	Output      []byte
	Error       string
	LatencyMs   int64
	TaintLevel  types.TaintLevel
	SandboxTier types.SandboxTier
	ImageParts  []types.ImagePart
	// MCPRaw 透传自 types.ToolResult.MCPRaw（MCP 工具的原始 CallToolResult JSON），
	// 供上层（InMemoryToolRegistry.ExecuteTool）继续透传给调用方。
	MCPRaw json.RawMessage
}

// PreToolUseResult PreToolUse 分发结果。
type PreToolUseResult struct {
	Blocked bool
	Reason  string
	// UpdatedInput hook 改写后的工具入参（两家 hookSpecificOutput.updatedInput）。Envelope 对改写后
	// 的请求重新执行 PolicyGate——策略是按原入参给出的 allow，不能沿用到被改写的入参上。
	UpdatedInput map[string]any
}

// HookFirer 供 ExecEnvelope 在工具调用前后触发 hooks.json 事件（ADR-0103 决策六）。
// 实现见 internal/action/hook.Runner；consumer-side 接口定义于此打破依赖环。
// Boot 通过 SetHookFirer 注入，nil 时 Execute 跳过 Hook 触发（不阻断主流程）。
type HookFirer interface {
	// FirePreToolUse veto-only：只能在 PolicyGate 已 allow 的基础上追加拒绝或改写入参，
	// hook 的 allow 不能推翻策略 deny（不构成第二策略引擎，HE-7）。
	FirePreToolUse(ctx context.Context, toolName string, toolInput map[string]any) PreToolUseResult
	// FirePostToolUse 成功走 PostToolUse、失败走 PostToolUseFailure。返回需回传模型的反馈
	// （decision:block 的 reason / additionalContext，两家语义）；不改写工具结果本身。
	FirePostToolUse(ctx context.Context, toolName string, toolInput map[string]any, output string, success bool, errMsg string) string
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

// PersistentSandboxAvailable 报告 D4/ADR-0008 长驻会话后端当前是否可用。
// 调用方（目前仅 code_act.go）必须在决定"要不要跳过 pickle 包装、直接发送
// 原始代码"之前同步查询此方法——这是打破"先写脚本文件、后决定走哪条 tier"
// 先有鸡先有蛋问题的关键：CodeAct 必须在构造脚本内容之前就确定性地知道最终
// 会不会用上 L4，而不能等 Execute() 内部路由完才知道。
func (e *ExecEnvelope) PersistentSandboxAvailable() bool {
	return e.router != nil && e.router.PersistentAvailable()
}

// RequiresCapabilityToken 返回该工具是否必须持有效能力令牌（inv_M7_01）。
// CapReadOnly 之上（CapWriteLocal / CapWriteNetwork / CapPrivileged）一律要求；
// 只读豁免理由：read_file/glob/grep 等大量内置只读工具不带 token，直接改会全线红。
//
//nolint:gocyclo
func RequiresCapabilityToken(c types.CapabilityLevel) bool {
	return c > types.CapReadOnly
}

// applyPreToolUse 执行 PreToolUse hook：阻断时返回失败结果；改写入参时替换 req.Input 并
// 重新执行 PolicyGate（策略按原入参放行，不能沿用到改写后的入参）。
func (e *ExecEnvelope) applyPreToolUse(ctx context.Context, req *ExecRequest, start time.Time) (*ExecResult, error) {
	if e.hookFirer == nil {
		return nil, nil
	}
	pre := e.hookFirer.FirePreToolUse(ctx, req.Resource, hookToolInput(*req))
	if pre.Blocked {
		slog.WarnContext(ctx, "exec_envelope: blocked by PreToolUse hook",
			"principal", req.Principal, "kind", req.Kind, "resource", req.Resource, "reason", pre.Reason)
		return &ExecResult{Success: false, Error: "exec_envelope: pre_tool_use hook blocked: " + pre.Reason,
			LatencyMs: time.Since(start).Milliseconds(), TaintLevel: req.TaintLevel}, nil
	}
	if pre.UpdatedInput == nil {
		return nil, nil
	}
	rewritten, err := json.Marshal(pre.UpdatedInput)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "exec_envelope: encode hook updated input", err)
	}
	req.Input = rewritten
	return e.authorize(ctx, *req, start), nil
}

// authorize PolicyGate 判定；拒绝时返回失败结果（nil 表示放行）。
func (e *ExecEnvelope) authorize(ctx context.Context, req ExecRequest, start time.Time) *ExecResult {
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
	if req.Principal == PrincipalMCPApp {
		// MCP Apps View 发起的调用：PolicyGate 可据此单独收紧（如禁止 app-only
		// 工具触碰高危 Capability），审计日志（下方 slog）同样需要这两个字段。
		evalCtx["app_view_id"] = req.AppViewID
		evalCtx["app_session_id"] = req.AppSessionID
	}
	allowed, pErr := e.policy.IsAuthorized(ctx, req.Principal, string(req.Kind), req.Resource, evalCtx)
	if pErr == nil && allowed {
		return nil
	}
	reason := "policy denied"
	if pErr != nil {
		reason = pErr.Error()
	}
	slog.WarnContext(ctx, "exec_envelope: policy denied",
		"principal", req.Principal, "kind", req.Kind, "resource", req.Resource,
		"trust_tier", int(req.TrustTier), "reason", reason)
	return &ExecResult{Success: false, Error: "exec_envelope: " + reason,
		LatencyMs: time.Since(start).Milliseconds(), TaintLevel: req.TaintLevel}
}

//nolint:gocyclo // A-7 Capability Token 校验覆盖非只读工具后复杂度 24，Execute 是单一责任的执行闸门，拆分会破坏线性事务语义
func (e *ExecEnvelope) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	start := time.Now()

	// Step 1: PolicyGate（deny-by-default）
	if e.policy == nil {
		return nil, apperr.New(apperr.CodeForbidden, "exec_envelope: policy gate not initialized (deny-by-default)")
	}
	if denied := e.authorize(ctx, req, start); denied != nil {
		return denied, nil
	}

	// Step 1.5: PreToolUse Hook（hook 自身经 sandbox.RunStdio 执行，不经本入口，无递归）。
	if denied, err := e.applyPreToolUse(ctx, &req, start); denied != nil || err != nil {
		return denied, err
	}

	// Step 2: 沙箱等级（信任 + 工具属性 → tier）
	actualTier, tierErr := AssignSandboxTier(req.Tool, req.TrustTier, e.hwTier, e.goos)
	if tierErr != nil {
		return nil, apperr.Wrap(apperr.CodeSandboxTier0Limit, "exec_envelope: sandbox tier rejected", tierErr)
	}
	// D4/ADR-0008：有状态会话请求覆盖为 SandboxPersistent tier。AssignSandboxTier
	// 是面向所有工具的通用信任/能力判定，不了解"这是一次有状态 CodeAct 会话"这一
	// 语义，因此在此单独覆盖。不是隔离降级——RouteByTier 的 SandboxPersistent
	// 分支在后端不可用时会退化到与 SandboxContainer 完全一致的降级链
	// （Container→Remote→fail-closed），隔离强度不低于 AssignSandboxTier 原本会
	// 分配的等级。
	if req.StatefulSession && req.SessionID != "" {
		actualTier = types.SandboxPersistent
	}

	// Step 3: Capability Token（Privileged/CapWrite 强制）
	if RequiresCapabilityToken(req.Tool.Capability) {
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
		ExtraEnv:    req.ExtraEnv,
		CPUQuotaMs:  req.CPUQuotaMs,
		IOBudget:    req.IOBudget,
		SystemTier:  e.hwTier,
		TaintLevel:  req.TaintLevel,
		DryRunMode:  req.DryRun,
		SessionID:   req.SessionID,
		Language:    req.Language,
	}
	execCtx, done := e.router.trackExec(ctx, req.Resource)
	toolResult, execErr := provider.Run(execCtx, spec)
	done()
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

	if req.Principal == PrincipalMCPApp {
		slog.InfoContext(ctx, "exec_envelope: executed (mcp-app)",
			"kind", req.Kind, "resource", req.Resource, "trust_tier", int(req.TrustTier),
			"actual_tier", int(actualTier), "taint", int(outTaint), "latency_ms", time.Since(start).Milliseconds(),
			"app_view_id", req.AppViewID, "app_session_id", req.AppSessionID)
	} else {
		slog.InfoContext(ctx, "exec_envelope: executed",
			"kind", req.Kind, "resource", req.Resource, "trust_tier", int(req.TrustTier),
			"actual_tier", int(actualTier), "taint", int(outTaint), "latency_ms", time.Since(start).Milliseconds())
	}

	// PostToolUse Hook：同步执行，反馈追加在输出之后并强制 TaintHigh（hook 输出不可信，
	// ADR-0103 决策六修订原"不回写"约束：两家标准中 PostToolUse 的反馈必须到达模型）。
	output := toolResult.Output
	if e.hookFirer != nil {
		feedback := e.hookFirer.FirePostToolUse(ctx, req.Resource, hookToolInput(req), string(toolResult.Output), toolResult.Success, toolResult.Error)
		if feedback != "" {
			output = append(append(append([]byte{}, output...), "\n\n<hook-feedback>\n"...), feedback+"\n</hook-feedback>"...)
			outTaint = types.TaintHigh
		}
	}

	return &ExecResult{
		Success: toolResult.Success, Output: output, Error: toolResult.Error,
		LatencyMs: time.Since(start).Milliseconds(), TaintLevel: outTaint,
		SandboxTier: actualTier, ImageParts: toolResult.ImageParts, MCPRaw: toolResult.MCPRaw,
	}, nil
}

// hookToolInput 两家 hook 的 tool_input 是工具参数对象：Input 为 JSON 对象时原样解码；
// 脚本类执行给出 script_path 字段，便于 matcher 与 "if" 规则匹配。
func hookToolInput(req ExecRequest) map[string]any {
	m := map[string]any{}
	if len(req.Input) > 0 && json.Unmarshal(req.Input, &m) != nil {
		m = map[string]any{"input": string(req.Input)}
	}
	if req.ScriptPath != "" {
		m["script_path"] = req.ScriptPath
	}
	return m
}
