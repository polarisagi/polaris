package agent

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ProviderRecoveryHandler 处理 M1 CircuitBreaker 恢复后的任务唤醒逻辑。
// 注册到 pkg/substrate/outbox_worker.go 的 OutboxWorker，
// 消费 target_engine="provider_recovery" 的 Outbox 事件。
//
// 两步恢复流程：
//  1. SessionPIIVault.RestoreFromSnapshot：将会话 PII 快照从临时存储恢复到 WorkingMemory
//  2. Blackboard.ResumeFromSuspended：将 suspended 状态任务重新投递到 Blackboard
type ProviderRecoveryHandler struct {
	piiVault   PIIVaultRestorer
	blackboard BlackboardResumer
}

// PIIVaultRestorer 从快照恢复 PII 数据的接口（consumer-side，防止包循环）。
type PIIVaultRestorer interface {
	RestoreFromSnapshot(ctx context.Context, taskID string) error
}

// BlackboardResumer 从 suspended 状态恢复任务到 Blackboard 的接口。
type BlackboardResumer interface {
	ResumeFromSuspended(ctx context.Context, taskID string) error
	PostTask(ctx context.Context, task *types.TaskEntry) error
}

// NewProviderRecoveryHandler 创建恢复处理器（依赖注入，nil 安全降级）。
func NewProviderRecoveryHandler(vault PIIVaultRestorer, blackboard BlackboardResumer) *ProviderRecoveryHandler {
	return &ProviderRecoveryHandler{
		piiVault:   vault,
		blackboard: blackboard,
	}
}

// SetBlackboard dynamically injects the blackboard into the handler.
func (h *ProviderRecoveryHandler) SetBlackboard(bb BlackboardResumer) {
	h.blackboard = bb
}

// SetPIIVault dynamically injects the PIIVault into the handler.
func (h *ProviderRecoveryHandler) SetPIIVault(vault PIIVaultRestorer) {
	h.piiVault = vault
}

// Handle 消费 provider_recovery Outbox 事件，执行两步恢复。
// 幂等：重复触发同一 taskID 安全，RestoreFromSnapshot 和 ResumeFromSuspended 均应幂等。
func (h *ProviderRecoveryHandler) Handle(ctx context.Context, payload []byte) error {
	var data struct {
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
		Priority  int    `json:"priority"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "recovery: failed to parse payload", err)
	}
	if data.TaskID == "" {
		return apperr.New(apperr.CodeInvalidInput, "recovery: task_id is empty")
	}

	slog.Info("kernel: recovering task after provider exhaustion",
		"task_id", data.TaskID, "session_id", data.SessionID)

	// Step 1: 恢复 PII 快照（nil 安全：vault 未注入时跳过）
	if h.piiVault != nil {
		if err := h.piiVault.RestoreFromSnapshot(ctx, data.TaskID); err != nil {
			// 非致命：PII 恢复失败不阻断任务唤醒，记录告警
			slog.Warn("kernel: pii vault restore failed",
				"task_id", data.TaskID, "err", err)
		}
	}

	// Step 2: 唤醒 suspended 任务
	if h.blackboard == nil {
		return apperr.New(apperr.CodeInternal, "recovery: blackboard not injected")
	}

	// 优先尝试 ResumeFromSuspended（任务已在 Blackboard 中处于 suspended 态）
	if err := h.blackboard.ResumeFromSuspended(ctx, data.TaskID); err != nil {
		// 降级：直接重新投递（任务可能已被 Reaper 清理）
		slog.Warn("kernel: resume suspended failed, re-posting task",
			"task_id", data.TaskID, "err", err)

		priority := data.Priority
		if priority == 0 {
			priority = 1 // 前台优先级
		}
		entry := types.TaskEntry{
			ID:        data.TaskID,
			Type:      "provider_recovered",
			Priority:  priority,
			ClaimedBy: data.AgentID,
		}
		if postErr := h.blackboard.PostTask(ctx, &entry); postErr != nil {
			return apperr.Wrap(apperr.CodeInternal, "recovery: re-post task failed", postErr)
		}
	}

	slog.Info("kernel: task recovery complete", "task_id", data.TaskID)
	return nil
}
