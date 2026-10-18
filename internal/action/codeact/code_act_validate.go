package codeact

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

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

func (ca *CodeAct) validatePolicyAndEnv(_ context.Context, req protocol.CodeActRequest) error {
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
	// 兑现 Claims.MaxCallsPerTask（2026-08-06 修复）：Verify 只覆盖签名/过期/撤销，
	// "还能用几次"是有状态语义，必须在真正放行副作用之前单独消费一次。
	// 本处是 code_act 唯一的执行入口，放在这里等价于"每次真实执行消费一次"。
	if consumeErr := ca.tokenMgr.Consume(req.CapabilityID); consumeErr != nil {
		return apperr.Wrap(apperr.CodeForbidden, "code_act: capability token consumption refused", consumeErr)
	}

	return nil
}

func (ca *CodeAct) validateAST(req protocol.CodeActRequest) error {
	if ca.astChecker == nil {
		return nil
	}
	var err error
	switch req.Language {
	case "python":
		err = ca.astChecker.CheckPython([]byte(req.Code))
	case "bash":
		err = ca.astChecker.CheckBash([]byte(req.Code))
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "code_act: L0 AST 检查失败", err)
	}
	return nil
}

func (ca *CodeAct) validateL1(req protocol.CodeActRequest) error {
	caps := map[string]bool{}
	if err := ca.govAgent.ValidateCode(req.Language, []byte(req.Code), caps); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "code_act: L1 governance 检查失败", err)
	}
	return nil
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
