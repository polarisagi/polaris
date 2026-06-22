package codeact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// CodeAct ad-hoc 代码执行引擎。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §7.4
//
// 区别于 Logic-Collapse（M06）:
//
//	Logic-Collapse: S2 结果 → 蒸馏为 Wasm → 持久化到 SkillRegistry（System 1 缓存）
//	CodeAct:        LLM 生成 Python/Bash → 一次性执行 → 结果返回（不持久化）
//
// 安全约束（inv_global_07）:
//   - 强制 Sbx-L3（ContainerSandbox），禁止降级为 L1/L2
//   - 必须携带有效 CapabilityToken
//   - 执行前 Cedar 策略评估（llm_generated forbid 规则阻断网络写入/部署）
//   - 全链路 Audit（写入 EventLog）
//
// sandbox 字段使用包内 SandboxProvider 接口（Run 签名与 types.SandboxSpec 不同）。
// LevelChecker 接口仅用于级别断言，由三个沙箱实现通过 Level() 满足。
type CodeAct struct {
	sandbox     sandbox.SandboxProvider
	policyGate  protocol.PolicyGate
	toolExec    protocol.ToolExecutor
	govAgent    govAgent        // 可选的安全校验网关 (L1)
	astChecker  ASTChecker      // L0 AST 检查器
	reviewer    LLMPeerReviewer // L2 LLM 同行评审
	hitlGateway protocol.HITL   // HITL 网关（处理警告级别）
}

type govAgent interface {
	ValidateCode(language string, code []byte, caps map[string]bool) error
}

// CodeActOption 定义初始化选项
type CodeActOption func(*CodeAct)

// WithGovernanceAgent 允许在初始化时注入治理守门人进行安全校验 (L1)
func WithGovernanceAgent(ga govAgent) CodeActOption {
	return func(c *CodeAct) {
		c.govAgent = ga
	}
}

// WithASTChecker 注入 L0 AST 检查器
func WithASTChecker(checker ASTChecker) CodeActOption {
	return func(c *CodeAct) {
		c.astChecker = checker
	}
}

// WithPeerReviewer 注入 L2 LLM 评审器
func WithPeerReviewer(reviewer LLMPeerReviewer) CodeActOption {
	return func(c *CodeAct) {
		c.reviewer = reviewer
	}
}

// WithHITL 注入 HITL Gateway
func WithHITL(gw protocol.HITL) CodeActOption {
	return func(c *CodeAct) {
		c.hitlGateway = gw
	}
}

// CodeActRequest CodeAct 执行请求。
type CodeActRequest struct {
	Language     string // "python" | "bash"
	Code         string // LLM 生成的代码文本
	CapabilityID string // 必须携带有效 CapabilityToken（inv_global_07）
	SessionID    string
	AgentID      string
	TaintLevel   types.TaintLevel
}

// CodeActResult CodeAct 执行结果。
type CodeActResult struct {
	Output    []byte
	ExitCode  int
	LatencyMs int64
}

func NewCodeAct(sandbox sandbox.SandboxProvider, policyGate protocol.PolicyGate, toolExec protocol.ToolExecutor, opts ...CodeActOption) *CodeAct {
	ca := &CodeAct{
		sandbox:    sandbox,
		policyGate: policyGate,
		toolExec:   toolExec,
	}
	for _, opt := range opts {
		opt(ca)
	}
	return ca
}

func (ca *CodeAct) validateExecuteRequest(ctx context.Context, req CodeActRequest) error {
	if err := ca.validateBasic(req); err != nil {
		return fmt.Errorf("CodeAct.validateExecuteRequest: %w", err)
	}
	if err := ca.validatePolicyAndEnv(ctx, req); err != nil {
		return fmt.Errorf("CodeAct.validateExecuteRequest: %w", err)
	}
	if err := ca.validateAST(req); err != nil {
		return fmt.Errorf("CodeAct.validateExecuteRequest: %w", err)
	}
	if err := ca.validateL1(req); err != nil {
		return fmt.Errorf("CodeAct.validateExecuteRequest: %w", err)
	}
	return ca.validateL2(ctx, req)
}

func (ca *CodeAct) validateBasic(req CodeActRequest) error {
	if req.Code == "" {
		return apperr.New(apperr.CodeInternal, "code_act: code is empty")
	}
	if req.Language != "python" && req.Language != "bash" {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("code_act: unsupported language %q (allowed: python|bash)", req.Language))
	}
	if req.CapabilityID == "" {
		return apperr.New(apperr.CodeForbidden, "code_act: capability_id required (inv_global_07)")
	}
	return nil
}

func (ca *CodeAct) validatePolicyAndEnv(ctx context.Context, req CodeActRequest) error {
	if ca.policyGate == nil {
		return apperr.New(apperr.CodeInternal, "code_act: policy gate not available (fail-closed)")
	}
	evalCtx := map[string]any{
		"source":              "llm_generated",  // 明确标识代码来源为大语言模型生成，触发专门的安全审计规则
		"capability_token_id": req.CapabilityID, // 强制验证能力令牌，防范越权操作
		"trust_level":         1,                // 信任等级固定为 1 (Untrusted/Local)，因为是动态生成的代码
	}
	allowed, err := ca.policyGate.IsAuthorized(ctx, req.AgentID, "execute_code", "code_act:"+req.Language, evalCtx)
	if err != nil || !allowed {
		reason := "policy denied"
		if err != nil {
			reason = err.Error()
		}
		return apperr.New(apperr.CodeForbidden, "code_act: policy gate denied: "+reason)
	}

	if ca.sandbox == nil {
		return apperr.New(apperr.CodeInternal, "code_act: sandbox not available (fail-closed)")
	}
	type levelProvider interface{ Level() int }
	if lp, ok := ca.sandbox.(levelProvider); !ok || lp.Level() < 3 {
		lvl := 0
		if lp, ok2 := ca.sandbox.(levelProvider); ok2 {
			lvl = lp.Level()
		}
		return apperr.New(apperr.CodeForbidden, fmt.Sprintf("code_act: sandbox level %d < required L3 (inv_global_07)", lvl))
	}
	if ca.govAgent == nil {
		return apperr.New(apperr.CodeInternal,
			"code_act: governance agent not initialized, refusing code execution (fail-closed)")
	}
	return nil
}

func (ca *CodeAct) validateAST(req CodeActRequest) error {
	if ca.astChecker == nil {
		return nil
	}
	if req.Language == "python" {
		return ca.astChecker.CheckPython([]byte(req.Code))
	} else if req.Language == "bash" {
		return ca.astChecker.CheckBash([]byte(req.Code))
	}
	return nil
}

func (ca *CodeAct) validateL1(req CodeActRequest) error {
	caps := map[string]bool{}
	return ca.govAgent.ValidateCode(req.Language, []byte(req.Code), caps)
}

func (ca *CodeAct) validateL2(ctx context.Context, req CodeActRequest) error {
	if req.TaintLevel < types.TaintHigh || ca.reviewer == nil {
		return nil
	}
	risk, err := ca.reviewer.Review(ctx, req.Code)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "code_act: L2 peer review failed", err)
	}
	if risk == "danger" {
		return apperr.New(apperr.CodeForbidden, "code_act: L2 peer review rejected (danger)")
	}
	if risk == "warning" {
		return ca.requestHITLForWarning(ctx, req)
	}
	return nil
}

func (ca *CodeAct) requestHITLForWarning(ctx context.Context, req CodeActRequest) error {
	if ca.hitlGateway == nil {
		return apperr.New(apperr.CodeForbidden, "code_act: L2 peer review rejected (warning - needs HITL, but no HITL gateway available)")
	}
	resp, err := ca.hitlGateway.Prompt(ctx, types.HITLPrompt{
		ID:             req.SessionID,
		CheckpointType: "code_act_warning",
		PromptText:     "LLM generated code flagged as warning. Approve execution?",
		Options: []types.HITLOption{
			{Key: "approve", Label: "Approve"},
			{Key: "deny", Label: "Deny"},
		},
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "code_act: HITL prompt failed", err)
	}
	if resp != nil && resp.Approved {
		return nil
	}
	return apperr.New(apperr.CodeForbidden, "code_act: L2 peer review rejected (warning - HITL denied)")
}

// Execute 执行 LLM 生成的代码（强制 Sbx-L3 + Cedar 门控）。
func (ca *CodeAct) Execute(ctx context.Context, req CodeActRequest) (*CodeActResult, error) {
	// 前置校验与权限检查
	if err := ca.validateExecuteRequest(ctx, req); err != nil {
		return nil, fmt.Errorf("CodeAct.Execute: %w", err)
	}

	// 构造沙箱运行规格
	// 安全策略：LLM 生成代码写入临时文件执行，禁止通过 -c 参数拼接（shell 注入向量）。
	// 原 `python3 -c <code>` 方式存在注入风险：代码中的引号/反斜杠可逃逸 shell 边界。
	// 临时文件路径使用随机后缀，避免路径预测攻击。
	tmpFile, err := writeTempScript(req.Language, req.Code)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: write temp script failed", err)
	}
	defer os.Remove(tmpFile) // 执行后立即删除，防止敏感代码驻留磁盘

	// ScriptPath 传递脚本路径，由 ContainerSandbox.runNativeScript() 推断解释器并执行。
	// 旧方案（Input = []byte("python3 /tmp/xxx.py")）的致命缺陷：ContainerSandbox 将
	// Input 字节流作为 stdin 传给 Firecracker/VZ 二进制，不会当作 shell 命令执行（R1.15）。
	spec := sandbox.SandboxSpec{
		ToolName:   "code_act:" + req.Language,
		ScriptPath: tmpFile,      // 直接告知沙箱脚本路径，解释器由后缀推断
		Input:      []byte("{}"), // stdin 留空（兼容协议格式，容器后端不使用此值）
		CPUQuotaMs: 30000,        // 30s 超时
	}

	result, err := ca.sandbox.Run(ctx, spec)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: sandbox execution failed", err)
	}

	exitCode := 0
	if !result.Success {
		exitCode = 1
	}

	// 全链路审计：写入 EventLog（inv_global_07 要求）
	if ca.toolExec != nil {
		auditPayload, _ := json.Marshal(map[string]any{
			"session_id":    req.SessionID,
			"agent_id":      req.AgentID,
			"language":      req.Language,
			"capability_id": req.CapabilityID,
			"taint_level":   req.TaintLevel,
			"exit_code":     exitCode,
			"latency_ms":    result.LatencyMs,
		})
		_ = ca.toolExec.RecordAudit(ctx, "code_act", auditPayload)
	}

	return &CodeActResult{
		Output:    result.Output,
		ExitCode:  exitCode,
		LatencyMs: result.LatencyMs,
	}, nil
}

// writeTempScript 将 LLM 生成代码写入临时文件，返回文件路径。
// 文件权限 0600（仅当前用户可读），后缀依语言区分（.py / .sh）。
func writeTempScript(language, code string) (string, error) {
	var ext string
	switch language {
	case "python":
		ext = "*.py"
	case "bash":
		ext = "*.sh"
	default:
		ext = "*.tmp"
	}

	f, err := os.CreateTemp("", "polaris_codeact_"+ext)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "create temp file", err)
	}
	defer f.Close()

	if _, err := f.WriteString(code); err != nil {
		os.Remove(f.Name())
		return "", apperr.Wrap(apperr.CodeInternal, "write temp file", err)
	}
	// 设置执行权限（bash 脚本需要）
	if language == "bash" {
		if err := os.Chmod(f.Name(), 0700); err != nil {
			os.Remove(f.Name())
			return "", apperr.Wrap(apperr.CodeInternal, "chmod temp file", err)
		}
	}
	return f.Name(), nil
}
