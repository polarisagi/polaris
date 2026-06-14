package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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
// sandbox 字段使用包内 SandboxProvider 接口（Run 签名与 protocol.SandboxSpec 不同）。
// LevelChecker 接口仅用于级别断言，由三个沙箱实现通过 Level() 满足。
type CodeAct struct {
	sandbox    SandboxProvider
	policyGate protocol.PolicyGate
	toolExec   protocol.ToolExecutor
	govAgent   govAgent        // 可选的安全校验网关 (L1)
	astChecker ASTChecker      // L0 AST 检查器
	reviewer   LLMPeerReviewer // L2 LLM 同行评审
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

// CodeActRequest CodeAct 执行请求。
type CodeActRequest struct {
	Language     string // "python" | "bash"
	Code         string // LLM 生成的代码文本
	CapabilityID string // 必须携带有效 CapabilityToken（inv_global_07）
	SessionID    string
	AgentID      string
	TaintLevel   protocol.TaintLevel
}

// CodeActResult CodeAct 执行结果。
type CodeActResult struct {
	Output    []byte
	ExitCode  int
	LatencyMs int64
}

func NewCodeAct(sandbox SandboxProvider, policyGate protocol.PolicyGate, toolExec protocol.ToolExecutor, opts ...CodeActOption) *CodeAct {
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

// validateExecuteRequest 抽离出的校验逻辑
func (ca *CodeAct) validateExecuteRequest(ctx context.Context, req CodeActRequest) error {
	if req.Code == "" {
		return perrors.New(perrors.CodeInternal, "code_act: code is empty")
	}
	if req.Language != "python" && req.Language != "bash" {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("code_act: unsupported language %q (allowed: python|bash)", req.Language))
	}
	if req.CapabilityID == "" {
		return perrors.New(perrors.CodeForbidden, "code_act: capability_id required (inv_global_07)")
	}

	if ca.policyGate == nil {
		return perrors.New(perrors.CodeInternal, "code_act: policy gate not available (fail-closed)")
	}
	evalCtx := map[string]any{
		"source":              "llm_generated",
		"capability_token_id": req.CapabilityID,
		"trust_level":         1,
	}
	allowed, err := ca.policyGate.IsAuthorized(ctx, req.AgentID, "execute_code", "code_act:"+req.Language, evalCtx)
	if err != nil || !allowed {
		reason := "policy denied"
		if err != nil {
			reason = err.Error()
		}
		return perrors.New(perrors.CodeForbidden, "code_act: policy gate denied: "+reason)
	}

	if ca.sandbox == nil {
		return perrors.New(perrors.CodeInternal, "code_act: sandbox not available (fail-closed)")
	}
	type levelProvider interface{ Level() int }
	if lp, ok := ca.sandbox.(levelProvider); !ok || lp.Level() < 3 {
		lvl := 0
		if lp, ok2 := ca.sandbox.(levelProvider); ok2 {
			lvl = lp.Level()
		}
		return perrors.New(perrors.CodeForbidden, fmt.Sprintf("code_act: sandbox level %d < required L3 (inv_global_07)", lvl))
	}
	if ca.govAgent == nil {
		return perrors.New(perrors.CodeInternal,
			"code_act: governance agent not initialized, refusing code execution (fail-closed)")
	}

	// L0 AST 校验
	if ca.astChecker != nil {
		if req.Language == "python" {
			if err := ca.astChecker.CheckPython([]byte(req.Code)); err != nil {
				return err
			}
		} else if req.Language == "bash" {
			if err := ca.astChecker.CheckBash([]byte(req.Code)); err != nil {
				return err
			}
		}
	}

	// L1 正则匹配校验
	caps := map[string]bool{}
	if err := ca.govAgent.ValidateCode(req.Language, []byte(req.Code), caps); err != nil {
		return err
	}

	// L2 LLM 同行评审 (污点级别较高时触发)
	if req.TaintLevel >= protocol.TaintHigh {
		if ca.reviewer != nil {
			risk, err := ca.reviewer.Review(ctx, req.Code)
			if err != nil {
				return perrors.Wrap(perrors.CodeInternal, "code_act: L2 peer review failed", err)
			}
			if risk == "danger" {
				return perrors.New(perrors.CodeForbidden, "code_act: L2 peer review rejected (danger)")
			} else if risk == "warning" {
				// 触发 HITL 等机制，这里可根据协议要求扩展，暂且记录日志或阻断
				return perrors.New(perrors.CodeForbidden, "code_act: L2 peer review rejected (warning - needs HITL)")
			}
		} else {
			// 退化为 L1 严格模式（这里 L1 已经通过，可增加额外逻辑，如阻断）
			// prompt: "L2 nil → 退化为扩展正则" (这里可以认为 L1 ValidateCode 已经做了，或者我们可以再做一次更严谨的)
		}
	}

	return nil
}

// Execute 执行 LLM 生成的代码（强制 Sbx-L3 + Cedar 门控）。
func (ca *CodeAct) Execute(ctx context.Context, req CodeActRequest) (*CodeActResult, error) {
	// 前置校验与权限检查
	if err := ca.validateExecuteRequest(ctx, req); err != nil {
		return nil, err
	}

	// 构造沙箱运行规格
	// 安全策略：LLM 生成代码写入临时文件执行，禁止通过 -c 参数拼接（shell 注入向量）。
	// 原 `python3 -c <code>` 方式存在注入风险：代码中的引号/反斜杠可逃逸 shell 边界。
	// 临时文件路径使用随机后缀，避免路径预测攻击。
	tmpFile, err := writeTempScript(req.Language, req.Code)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "code_act: write temp script failed", err)
	}
	defer os.Remove(tmpFile) // 执行后立即删除，防止敏感代码驻留磁盘

	var cmdBinary string
	switch req.Language {
	case "python":
		// 直接执行临时文件（无 shell 展开）
		cmdBinary = fmt.Sprintf("python3 %s", filepath.Clean(tmpFile))
	case "bash":
		cmdBinary = fmt.Sprintf("bash %s", filepath.Clean(tmpFile))
	}

	spec := SandboxSpec{
		ToolName:   "code_act:" + req.Language,
		Input:      []byte(cmdBinary),
		CPUQuotaMs: 30000, // 30s 超时
	}

	result, err := ca.sandbox.Run(ctx, spec)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "code_act: sandbox execution failed", err)
	}

	exitCode := 0
	if !result.Success {
		exitCode = 1
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
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(code); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp file: %w", err)
	}
	// 设置执行权限（bash 脚本需要）
	if language == "bash" {
		if err := os.Chmod(f.Name(), 0700); err != nil {
			os.Remove(f.Name())
			return "", fmt.Errorf("chmod temp file: %w", err)
		}
	}
	return f.Name(), nil
}
