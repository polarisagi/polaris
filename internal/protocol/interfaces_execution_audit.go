package protocol

import "context"

// ExecutionAuditIntent 描述一次即将发生的物理执行。Args 仅供消费端计算长度和摘要，
// 实现不得持久化其原文。
// @consumer: internal/sandbox, internal/extension/skill, internal/action/hook
// @producer: internal/security.ExecutionAuditGate
type ExecutionAuditIntent struct {
	Tool   string
	Source string
	Args   []byte
}

// ExecutionAuditor 在物理副作用边界先记意图、再记结果。实现不得在完成记录失败后重跑动作。
// @consumer: internal/sandbox, internal/extension/skill, internal/action/hook
// @producer: internal/security.ExecutionAuditGate
type ExecutionAuditor interface {
	BeginExecution(ctx context.Context, intent ExecutionAuditIntent) (executionID string, err error)
	CompleteExecution(ctx context.Context, executionID string, success bool, errorCode string) error
}
