package codeact

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/security/taint"
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
	envelope       *sandbox.ExecEnvelope
	auditor        AuditRecorder          // inv_global_07 全链路审计；nil 时跳过（仅测试）
	govAgent       govAgent               // 可选的安全校验网关 (L1)
	astChecker     ASTChecker             // L0 AST 检查器
	reviewer       LLMPeerReviewer        // L2 LLM 同行评审
	hitlGateway    protocol.HITL          // HITL 网关（处理警告级别）
	maxCodeSize    int                    // 强制的最大代码字节数（inv_global_07）
	desensitizer   *guard.PIIDesensitizer // PII 脱敏器
	detector       *guard.PIIDetector     // PII 检测器
	tokenMgr       tokenManager           // Capability Token 管理器
	stateDir       string                 // GD-4-002: REPL 状态快照根目录；空值时 StatefulSession 请求静默降级为一次性执行
	stagingBackend ScriptStagingBackend   // 批次4 XR-11 修复：临时脚本落盘目标；nil 时降级为 os.CreateTemp（见 code_act_script_staging.go）
	toolExec       ToolBridgeExecutor     // ADR-0115 WP5: 工具桥执行器
	policy         policy.Policy          // ADR-0115 WP5: 安全策略
}

type govAgent interface {
	ValidateCode(language string, code []byte, caps map[string]bool) error
}

type tokenManager interface {
	Lookup(tokenID string) (*token.Token, error)
	Verify(tok *token.Token) error
	// Consume 兑现 Claims.MaxCallsPerTask 的一次使用配额。Verify 只覆盖
	// 签名/过期/撤销这些无状态判定，"还剩几次可用"必须单独消费。
	Consume(tokenID string) error
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

// AuditRecorder CodeAct 审计写入的消费端接口（生产由 security.AuditTrail 满足）。
//
// 2026-09-20（GR-4.2-005 复核）：此前依赖整个 protocol.ToolExecutor（4 个方法）却只用
// RecordAudit，而全仓无生产类型同时实现这 4 个方法，装配点只能传 nil——inv_global_07
// 要求的 CodeAct 全链路审计在生产中从未写入过一条。收窄为单方法接口后直接注入 AuditTrail。
type AuditRecorder interface {
	RecordAudit(ctx context.Context, toolName string, payload []byte) error
}

func NewCodeAct(envelope *sandbox.ExecEnvelope, auditor AuditRecorder, opts ...CodeActOption) *CodeAct {
	ca := &CodeAct{
		envelope: envelope,
		auditor:  auditor,
	}
	for _, opt := range opts {
		opt(ca)
	}
	if ca.policy.IsZero() {
		ca.policy = policy.ProfilePolicy(policy.ProfileStandard)
	}
	return ca
}

// Execute 执行 LLM 生成的代码（强制 Sbx-L3 + Cedar 门控）。
func (ca *CodeAct) Execute(ctx context.Context, req protocol.CodeActRequest) (*protocol.CodeActResult, error) {
	// 前置校验与权限检查（实现移至 code_act_validate.go，R7 行数治理）
	if err := ca.validateExecuteRequest(ctx, req); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "CodeAct.Execute", err)
	}

	// ADR-0115 WP5: 启动专有工具桥服务（T6 原则）
	bridgeSrv := ca.startBridgeIfEligible(ctx, req)
	if bridgeSrv != nil {
		defer bridgeSrv.Close()
	}

	execCode, useL4, err := ca.prepareExecCode(req)
	if err != nil {
		return nil, err
	}

	// 安全策略：LLM 生成代码写入临时文件执行，禁止通过 -c 参数拼接（shell 注入向量）。
	// 原 `python3 -c <code>` 方式存在注入风险：代码中的引号/反斜杠可逃逸 shell 边界。
	// 临时文件路径使用随机后缀，避免路径预测攻击。stageScript 优先落盘至 VFS
	// 隔离边界内（ca.stagingBackend 非 nil 时），未注入时降级为系统 /tmp（批次4
	// XR-11 finding 复核修复，见 code_act_script_staging.go）。
	tmpFile, cleanupScript, err := ca.stageScript(req, execCode)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: write temp script failed", err)
	}
	defer cleanupScript() // 执行后立即删除，防止敏感代码驻留磁盘

	// 确保同目录存在 polaris.py 模块，供 Python 脚本直接 import polaris
	if bridgeSrv != nil {
		if perr := EnsurePolarisPythonModule(filepath.Dir(tmpFile)); perr != nil {
			slog.WarnContext(ctx, "codeact: write polaris.py failed", "err", perr)
		}
	}

	tok, _ := ca.tokenMgr.Lookup(req.CapabilityID)

	var extraEnv []string
	var allowedPaths []string
	if bridgeSrv != nil {
		extraEnv = append(extraEnv, "POLARIS_BRIDGE_SOCKET="+bridgeSrv.SocketPath())
		extraEnv = append(extraEnv, "POLARIS_BRIDGE_TOKEN="+bridgeSrv.Token())
		allowedPaths = append(allowedPaths, filepath.Dir(bridgeSrv.SocketPath()))
	}

	execReq := sandbox.ExecRequest{
		Principal: sandbox.PrincipalAgent, Kind: sandbox.KindScriptExecute,
		Resource: "codeact:" + req.Language, TrustTier: types.TrustUntrusted,
		// SideProcessSpawn 强制 AssignSandboxTier 分配 Sbx-L3（inv_global_07，GR-4.2-001）：
		// 仅标 ToolLLMGenerated 只会得到 SandboxWasm——wasmtime 在场时脚本被送进
		// WasmtimeExecute（ScriptBytes 为空，必然失败），不在场时才偶然回落到 Container。
		Tool: types.Tool{Name: "codeact:" + req.Language, Source: types.ToolLLMGenerated,
			SideEffects: []types.SideEffect{types.SideProcessSpawn}},
		Input: []byte("{}"), ScriptPath: tmpFile,
		CapToken:   tok,
		TaintLevel: types.TaintHigh, CPUQuotaMs: 30000,
		AllowedPaths: allowedPaths,
		ExtraEnv:     extraEnv,
	}
	if useL4 {
		// 只有真正按 L4 路径构造了 execCode（未经 pickle 包装）时才要求 Execute()
		// 把 tier 覆盖为 SandboxPersistent；useL4 已经在上面同步确认过
		// PersistentSandboxAvailable()，这里不会出现"想用 L4 但其实不可用"的
		// 不一致状态。
		execReq.SessionID = req.SessionID
		execReq.Language = req.Language
		execReq.StatefulSession = true
	}

	res, err := ca.envelope.Execute(ctx, execReq)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: sandbox execution failed", err)
	}

	highestTaint := types.TaintHigh
	if bridgeSrv != nil && bridgeSrv.HighestTaint() > highestTaint {
		highestTaint = bridgeSrv.HighestTaint()
	}

	return ca.finalizeExecuteResult(ctx, req, res, highestTaint)
}

// finalizeExecuteResult 计算退出码、二次 PII 脱敏输出、写入全链路审计并组装最终结果
// （从 Execute 拆出，gocyclo 治理，行为不变）。
func (ca *CodeAct) finalizeExecuteResult(ctx context.Context, req protocol.CodeActRequest, res *sandbox.ExecResult, finalTaint types.TaintLevel) (*protocol.CodeActResult, error) {
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
		redacted, _, err := ca.detector.RedactWithMode(ctx, string(out), "replace", req.SessionID, ca.desensitizer, nil)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "codeact: output PII desensitization failed, fail-closed", err)
		}
		out = []byte(redacted)
	}

	// 全链路审计：写入 EventLog（inv_global_07 要求）
	if ca.auditor != nil {
		// M07 §7.4：审计须含完整代码与输出（输出为上方二次 PII 脱敏后的版本）。
		auditPayload, _ := json.Marshal(map[string]any{
			"code":          req.Code,
			"output":        string(out),
			"session_id":    req.SessionID,
			"agent_id":      req.AgentID,
			"language":      req.Language,
			"capability_id": req.CapabilityID,
			"taint_level":   finalTaint,
			"exit_code":     exitCode,
			"latency_ms":    res.LatencyMs,
		})
		if auditErr := ca.auditor.RecordAudit(ctx, "code_act", auditPayload); auditErr != nil {
			slog.Error("codeact: audit record failed, compliance trail broken",
				"err", auditErr)
		}
	}

	return &protocol.CodeActResult{
		Output:    taint.NewTaintedString(string(out), taint.TaintSource{Module: "action/codeact", OriginTaintLevel: finalTaint}, "codeact_sandbox"),
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

	writeErr := func() error {
		defer f.Close()
		if _, err := f.WriteString(code); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "write temp file", err)
		}
		// 设置执行权限（bash 脚本需要）
		if language == "bash" {
			if err := os.Chmod(f.Name(), 0700); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "chmod temp file", err)
			}
		}
		return nil
	}()

	if writeErr != nil {
		os.Remove(f.Name())
		return "", writeErr
	}

	return f.Name(), nil
}

func (ca *CodeAct) startBridgeIfEligible(ctx context.Context, req protocol.CodeActRequest) *BridgeServer {
	if req.Language != "python" || ca.toolExec == nil {
		return nil
	}
	var opts []BridgeOption
	if checker := protocol.ProfileCheckerFromContext(ctx); checker != nil {
		opts = append(opts, WithBridgeProfileChecker(checker))
	}
	bs, berr := StartBridgeServer(ctx, ca.toolExec, ca.policy, types.TaintHigh, opts...)
	if berr != nil {
		slog.WarnContext(ctx, "codeact: start tool bridge failed, proceeding without bridge", "err", berr)
		return nil
	}
	return bs
}

func (ca *CodeAct) prepareExecCode(req protocol.CodeActRequest) (string, bool, error) {
	useL4 := req.StatefulSession && req.SessionID != "" &&
		(req.Language == "python" || req.Language == "bash") &&
		ca.envelope.PersistentSandboxAvailable()

	if useL4 {
		return req.Code, true, nil
	}
	code, err := ca.buildExecutableScript(req)
	if err != nil {
		return "", false, apperr.Wrap(apperr.CodeInternal, "code_act: build executable script failed", err)
	}
	return code, false, nil
}
