package codeact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/token"
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
	envelope     *sandbox.ExecEnvelope
	toolExec     protocol.ToolExecutor
	govAgent     govAgent               // 可选的安全校验网关 (L1)
	astChecker   ASTChecker             // L0 AST 检查器
	reviewer     LLMPeerReviewer        // L2 LLM 同行评审
	hitlGateway  protocol.HITL          // HITL 网关（处理警告级别）
	maxCodeSize  int                    // 强制的最大代码字节数（inv_global_07）
	desensitizer *guard.PIIDesensitizer // PII 脱敏器
	detector     *guard.PIIDetector     // PII 检测器
	tokenMgr     tokenManager           // Capability Token 管理器
	stateDir     string                 // GD-4-002: REPL 状态快照根目录；空值时 StatefulSession 请求静默降级为一次性执行
}

type govAgent interface {
	ValidateCode(language string, code []byte, caps map[string]bool) error
}

type tokenManager interface {
	Lookup(tokenID string) (*token.Token, error)
	Verify(tok *token.Token) error
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

// WithMaxCodeSize 注入强制的最大代码尺寸上限
func WithMaxCodeSize(bytes int) CodeActOption {
	return func(c *CodeAct) {
		c.maxCodeSize = bytes
	}
}

// WithPIIGuard 注入 PII 脱敏组件
func WithPIIGuard(detector *guard.PIIDetector, desensitizer *guard.PIIDesensitizer) CodeActOption {
	return func(c *CodeAct) {
		c.detector = detector
		c.desensitizer = desensitizer
	}
}

// WithTokenManager 注入 Capability Token 管理器
func WithTokenManager(mgr tokenManager) CodeActOption {
	return func(c *CodeAct) {
		c.tokenMgr = mgr
	}
}

// CodeActRequest CodeAct 执行请求。

func NewCodeAct(envelope *sandbox.ExecEnvelope, toolExec protocol.ToolExecutor, opts ...CodeActOption) *CodeAct {
	ca := &CodeAct{
		envelope: envelope,
		toolExec: toolExec,
	}
	for _, opt := range opts {
		opt(ca)
	}
	return ca
}

func (ca *CodeAct) validateExecuteRequest(ctx context.Context, req protocol.CodeActRequest) error {
	if err := ca.validateBasic(req); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CodeAct.validateExecuteRequest", err)
	}
	if err := ca.validatePolicyAndEnv(ctx, req); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CodeAct.validateExecuteRequest", err)
	}
	if err := ca.validateAST(req); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CodeAct.validateExecuteRequest", err)
	}
	if err := ca.validateL1(req); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CodeAct.validateExecuteRequest", err)
	}
	return ca.validateL2(ctx, req)
}

func (ca *CodeAct) validateBasic(req protocol.CodeActRequest) error {
	if req.Code == "" {
		return apperr.New(apperr.CodeInternal, "code_act: code is empty")
	}
	if req.Language != "python" && req.Language != "bash" {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("code_act: unsupported language %q (allowed: python|bash)", req.Language))
	}
	if req.CapabilityID == "" {
		return apperr.New(apperr.CodeForbidden, "code_act: capability_id required (inv_global_07)")
	}
	if ca.maxCodeSize > 0 && len(req.Code) > ca.maxCodeSize {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("code_act: code size %d bytes exceeds maximum limit of %d bytes", len(req.Code), ca.maxCodeSize))
	}
	return nil
}

func (ca *CodeAct) validatePolicyAndEnv(ctx context.Context, req protocol.CodeActRequest) error {
	if ca.envelope == nil {
		return apperr.New(apperr.CodeInternal, "code_act: envelope not available (fail-closed)")
	}
	if ca.govAgent == nil {
		return apperr.New(apperr.CodeInternal,
			"code_act: governance agent not initialized, refusing code execution (fail-closed)")
	}
	if ca.tokenMgr == nil {
		return apperr.New(apperr.CodeInternal, "code_act: token manager not initialized (fail-closed)")
	}

	tok, err := ca.tokenMgr.Lookup(req.CapabilityID)
	if err != nil {
		return apperr.New(apperr.CodeForbidden, "code_act: invalid capability token")
	}
	if verifyErr := ca.tokenMgr.Verify(tok); verifyErr != nil {
		return apperr.New(apperr.CodeForbidden, "code_act: capability token verification failed")
	}

	return nil
}

func (ca *CodeAct) validateAST(req protocol.CodeActRequest) error {
	if ca.astChecker == nil {
		return nil
	}
	switch req.Language {
	case "python":
		return ca.astChecker.CheckPython([]byte(req.Code))
	case "bash":
		return ca.astChecker.CheckBash([]byte(req.Code))
	}
	return nil
}

func (ca *CodeAct) validateL1(req protocol.CodeActRequest) error {
	caps := map[string]bool{}
	return ca.govAgent.ValidateCode(req.Language, []byte(req.Code), caps)
}

func (ca *CodeAct) validateL2(ctx context.Context, req protocol.CodeActRequest) error {
	// 注意：不能用 req.TaintLevel 做跳过判断——TaintLevel 来自调用方（LLM tool-call
	// JSON 参数 / HTTP 请求体，见 agent_execute.go / handler_codeact.go），
	// 是调用方自报的值，可被伪造成低污点从而绕过 L2。CodeAct 的存在意义就是执行
	// "LLM 生成的代码"，按定义恒为最高风险（Execute() 内构造 sandbox.ExecRequest
	// 时已硬编码 TaintLevel:TaintHigh，不受 req.TaintLevel 影响）——L2 语义审查同理，
	// 只要配置了 reviewer 就必须跑，不能被调用方声明的污点等级绕开。
	if ca.reviewer == nil {
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

func (ca *CodeAct) requestHITLForWarning(ctx context.Context, req protocol.CodeActRequest) error {
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
func (ca *CodeAct) Execute(ctx context.Context, req protocol.CodeActRequest) (*protocol.CodeActResult, error) {
	// 前置校验与权限检查
	if err := ca.validateExecuteRequest(ctx, req); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "CodeAct.Execute", err)
	}

	// 构造沙箱运行规格
	// GD-4-002: StatefulSession=true 时在真正执行的脚本首尾注入状态快照样板
	// （不改变 req.Code 本身，L0/L1/L2 审查已在上面 validateExecuteRequest 中
	// 针对原始 req.Code 完成，此处只影响实际执行的字节，不影响审查范围）。
	execCode, err := ca.buildExecutableScript(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: build executable script failed", err)
	}

	// 安全策略：LLM 生成代码写入临时文件执行，禁止通过 -c 参数拼接（shell 注入向量）。
	// 原 `python3 -c <code>` 方式存在注入风险：代码中的引号/反斜杠可逃逸 shell 边界。
	// 临时文件路径使用随机后缀，避免路径预测攻击。
	tmpFile, err := writeTempScript(req.Language, execCode)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: write temp script failed", err)
	}
	defer os.Remove(tmpFile) // 执行后立即删除，防止敏感代码驻留磁盘

	tok, _ := ca.tokenMgr.Lookup(req.CapabilityID)

	res, err := ca.envelope.Execute(ctx, sandbox.ExecRequest{
		Principal: sandbox.PrincipalAgent, Kind: sandbox.KindScriptExecute,
		Resource: "codeact:" + req.Language, TrustTier: types.TrustUntrusted,
		Tool:  types.Tool{Name: "codeact:" + req.Language, Source: types.ToolLLMGenerated},
		Input: []byte("{}"), ScriptPath: tmpFile,
		CapToken:   tok,
		TaintLevel: types.TaintHigh, CPUQuotaMs: 30000,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: sandbox execution failed", err)
	}

	exitCode := 0
	out := res.Output
	if !res.Success {
		exitCode = 1
		if res.Error != "" {
			if len(out) > 0 {
				out = append(out, '\n')
			}
			out = append(out, []byte(res.Error)...)
		}
	}

	// Task 8: CodeAct 输出端二次 PII 脱敏 (RedactWithMode)
	// fail-closed（2026-07-04 审计修复）：原实现 err != nil 时什么都不做，直接沿用
	// 未脱敏的原始 out 继续写入 EventLog 审计并返回给调用方，是静默 fail-open。
	// 脱敏本身失败说明无法保证输出不含明文 PII，必须整体拒绝而不是裸传原文。
	if ca.detector != nil && len(out) > 0 {
		redacted, _, err := ca.detector.RedactWithMode(ctx, string(out), "replace", ca.desensitizer, nil)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "codeact: output PII desensitization failed, fail-closed", err)
		}
		out = []byte(redacted)
	}

	// 全链路审计：写入 EventLog（inv_global_07 要求）
	if ca.toolExec != nil {
		auditPayload, _ := json.Marshal(map[string]any{
			"session_id":    req.SessionID,
			"agent_id":      req.AgentID,
			"language":      req.Language,
			"capability_id": req.CapabilityID,
			"taint_level":   types.TaintHigh, // 与 Execute() 内 sandbox.ExecRequest 的强制值保持一致，
			"exit_code":     exitCode,
			"latency_ms":    res.LatencyMs,
		})
		_ = ca.toolExec.RecordAudit(ctx, "code_act", auditPayload)
	}

	return &protocol.CodeActResult{
		Output:    out,
		ExitCode:  exitCode,
		LatencyMs: res.LatencyMs,
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
