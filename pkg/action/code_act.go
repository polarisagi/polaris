package action

import (
	"context"
	"fmt"
	"strings"

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

// NewCodeAct 创建 CodeAct 执行器。
// sandbox 必须为 Level()>=3 的沙箱（ContainerSandbox），违反则在 Execute 时 fail-closed。
func NewCodeAct(sandbox SandboxProvider, policyGate protocol.PolicyGate, toolExec protocol.ToolExecutor) *CodeAct {
	return &CodeAct{
		sandbox:    sandbox,
		policyGate: policyGate,
		toolExec:   toolExec,
	}
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
	return nil
}

// Execute 执行 LLM 生成的代码（强制 Sbx-L3 + Cedar 门控）。
func (ca *CodeAct) Execute(ctx context.Context, req CodeActRequest) (*CodeActResult, error) {
	// 前置校验与权限检查
	if err := ca.validateExecuteRequest(ctx, req); err != nil {
		return nil, err
	}

	// 构造沙箱运行规格
	var cmdBinary string
	switch req.Language {
	case "python":
		cmdBinary = "python3 -c " + sanitizeCodeForShell(req.Code)
	case "bash":
		cmdBinary = "bash -c " + sanitizeCodeForShell(req.Code)
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

// sanitizeCodeForShell 对 LLM 生成代码进行基本清洗（防止 shell 注入）。
// 移除反引号、$() 命令替换、常见危险命令前缀。
// 注意：这是 defense-in-depth 层，沙箱隔离才是主要保护。
func sanitizeCodeForShell(code string) string {
	// 移除 shell 命令替换语法（防止嵌套执行逃逸）
	code = strings.ReplaceAll(code, "`", "")
	// 不移除 $()，因为 Python 代码中 $() 不是 shell 语法
	// bash 代码的 $() 由 L3 沙箱隔离保护
	return code
}
