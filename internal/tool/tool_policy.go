// Package tool 提供 M7 工具注册表与执行管理。
// 本文件承载 ADR-0115 WP1 信任档位策略（Policy）与写前快照（Checkpoint）集成。
package tool

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// CheckpointTrigger 出口快照触发器接口（consumer-side 定义，防包循环）。
type CheckpointTrigger interface {
	MaybeSnapshot(ctx context.Context, toolName string, toolCap types.CapabilityLevel, sideEffects []types.SideEffect, input []byte, taintLevel types.TaintLevel) error
}

// WithPolicy 注入安全策略快照（ADR-0115 WP1）。
func (r *InMemoryToolRegistry) WithPolicy(p policy.Policy) *InMemoryToolRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policy = p
	return r
}

// WithCheckpointTrigger 注入写前快照触发器（仅 full 档生效，ADR-0115 WP1）。
func (r *InMemoryToolRegistry) WithCheckpointTrigger(t CheckpointTrigger) *InMemoryToolRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkpointTrigger = t
	return r
}

// checkAnomaly 检查工具输入是否异常，并根据策略执行 HITL 审批。
// 在 full 信任档位下（Policy.HITLAutoApprove=true），自动放行并记录审计日志。
func (r *InMemoryToolRegistry) checkAnomaly(ctx context.Context, name string, execInput []byte) error {
	f, ok := ctx.Value(protocol.CtxAnomalyFilterKey{}).(*guard.AnomalyDistanceFilter)
	if !ok {
		return nil
	}
	features := []float64{float64(len(execInput))}
	_, chkErr := f.Check(name, features)
	if chkErr == nil {
		return nil
	}

	r.mu.RLock()
	hitlGw := r.hitl
	pol := r.policy
	r.mu.RUnlock()

	// full 档：自动放行 + 审计（ADR-0115 WP1 行为矩阵）
	if pol.HITLAutoApprove {
		slog.Warn("tool_registry: anomaly filter HITL auto-approved (full profile)",
			"tool", name, "profile", pol.Profile)
		f.Record(name, features)
		return nil
	}

	if hitlGw == nil {
		return apperr.Wrap(apperr.CodeForbidden, "tool_registry: anomaly detected but no HITL gateway configured", chkErr)
	}

	taskID, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	resp, promptErr := hitlGw.Prompt(ctx, types.HITLPrompt{
		ID:             taskID,
		CheckpointType: "anomaly_escalation",
		PromptText:     fmt.Sprintf("Tool %q anomalous behavior detected. Approve execution?", name),
		Options: []types.HITLOption{
			{Key: "approve", Label: "Approve"},
			{Key: "deny", Label: "Deny"},
		},
	})
	if promptErr != nil {
		return apperr.Wrap(apperr.CodeInternal, "tool_registry: HITL prompt failed", promptErr)
	}
	if resp == nil || !resp.Approved {
		return apperr.New(apperr.CodeForbidden, "tool_registry: execution blocked by anomaly filter (HITL denied)")
	}
	slog.Info("tool_registry: HITL approved anomalous execution", "tool", name, "taskID", taskID)
	f.Record(name, features)
	return nil
}

// triggerPreExecutionCheckpoint 在满足条件时触发写前快照（仅 full 档生效）。
func (r *InMemoryToolRegistry) triggerPreExecutionCheckpoint(ctx context.Context, tool types.Tool, execInput []byte, taintLevel types.TaintLevel) {
	r.mu.RLock()
	ckpt := r.checkpointTrigger
	pol := r.policy
	r.mu.RUnlock()

	if pol.CheckpointEnabled && ckpt != nil {
		if snapErr := ckpt.MaybeSnapshot(ctx, tool.Name, tool.Capability, tool.SideEffects, execInput, taintLevel); snapErr != nil {
			slog.WarnContext(ctx, "tool_policy: checkpoint snapshot failed", "tool", tool.Name, "err", snapErr)
		}
	}
}
