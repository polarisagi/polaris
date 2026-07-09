package analysis

import (
	"github.com/polarisagi/polaris/internal/eval/harness"

	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type IncidentPayload struct {
	Input           map[string]any `json:"input"`
	Expected        map[string]any `json:"expected"`
	Actual          map[string]any `json:"actual"`
	Details         string         `json:"details"`
	NeedsHumanAudit bool           `json:"needs_human_audit"`
	TaintLevel      int            `json:"taint_level"`
}

type PIIDetector interface {
	Scrub(text string) string
}

type IncidentToEvalConverter struct {
	store       *harness.SQLiteEvalStore
	piiDetector PIIDetector
}

func NewIncidentToEvalConverter(store *harness.SQLiteEvalStore, pii PIIDetector) *IncidentToEvalConverter {
	return &IncidentToEvalConverter{store: store, piiDetector: pii}
}

func (c *IncidentToEvalConverter) Convert(ctx context.Context, incidentJSON []byte) (*harness.EvalCase, error) {
	var payload IncidentPayload
	if err := json.Unmarshal(incidentJSON, &payload); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to parse incident payload", err)
	}

	// 安全门控：needs_human_audit=true 或 taint_level>=3 的事件不自动入库
	if payload.NeedsHumanAudit || payload.TaintLevel >= 3 {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("incident requires human audit before eval conversion (taint=%d, needs_human_audit=%v)",
			payload.TaintLevel, payload.NeedsHumanAudit))
	}

	// PII 脱敏：对 Details 字段执行正则/模式替换
	details := payload.Details
	if c.piiDetector != nil {
		details = c.piiDetector.Scrub(details)
	}

	evalCase := &harness.EvalCase{
		ID:           fmt.Sprintf("incident_%d", time.Now().UnixNano()),
		Name:         "Auto-generated from Incident",
		Description:  details,
		Input:        payload.Input,
		Expected:     payload.Expected,
		Level:        harness.Level4LLMJudge,
		Severity:     harness.SeverityP0,
		BehaviorType: harness.BehaviorSemanticQuality,
	}

	// 写入 pending_review 分区，由人工审核后再迁移
	partition := "pending_review"
	if c.store != nil {
		if err := c.store.PutCase(ctx, partition, "incident", *evalCase); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to save eval case", err)
		}
	}

	return evalCase, nil
}

// ReviewAndPromote Phase 2 专家标注：将 pending_review 中的 case 经人工确认后迁移至 validation。
// reviewer: 审批人标识（审计用）；adjustedSeverity 允许专家降级（P0→P1/P2）。
// agentRole 必须与 PutCase 写入时的 agentRole 一致（例如 IncidentToEvalConverter.Convert 写入的是 "incident"）。
// 此方法需要在 API 层暴露给人工审核界面（M12 §6 HITL 入口）。
func (c *IncidentToEvalConverter) ReviewAndPromote(ctx context.Context, caseID, reviewer, agentRole string, adjustedSeverity harness.Severity) error {
	if reviewer == "" {
		return apperr.New(apperr.CodeInternal, "reviewer is required for Phase 2 annotation")
	}
	if c.store == nil {
		return apperr.New(apperr.CodeInternal, "store is nil")
	}
	// 1. 从 pending_review 读取 case
	// 2. 更新 harness.Severity（专家可降级，但不可升级超过 P0）
	// 3. 写入 validation 分区
	// 4. 记录审计事件（ reviewer、timestamp、adjustedSeverity 可由审计组件或者上层处理）
	// 5. 从 pending_review 删除
	return c.store.PromotePendingCase(ctx, caseID, "pending_review", "validation", reviewer, agentRole, adjustedSeverity)
}
