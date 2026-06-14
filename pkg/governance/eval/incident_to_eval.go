package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
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
	store       *SQLiteEvalStore
	piiDetector PIIDetector
}

func NewIncidentToEvalConverter(store *SQLiteEvalStore, pii PIIDetector) *IncidentToEvalConverter {
	return &IncidentToEvalConverter{store: store, piiDetector: pii}
}

func (c *IncidentToEvalConverter) Convert(ctx context.Context, incidentJSON []byte) (*EvalCase, error) {
	var payload IncidentPayload
	if err := json.Unmarshal(incidentJSON, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse incident payload: %w", err)
	}

	// 安全门控：needs_human_audit=true 或 taint_level>=3 的事件不自动入库
	if payload.NeedsHumanAudit || payload.TaintLevel >= 3 {
		return nil, fmt.Errorf("incident requires human audit before eval conversion (taint=%d, needs_human_audit=%v)",
			payload.TaintLevel, payload.NeedsHumanAudit)
	}

	// PII 脱敏：对 Details 字段执行正则/模式替换
	details := payload.Details
	if c.piiDetector != nil {
		details = c.piiDetector.Scrub(details)
	}

	evalCase := &EvalCase{
		ID:           fmt.Sprintf("incident_%d", time.Now().UnixNano()),
		Name:         "Auto-generated from Incident",
		Description:  details,
		Input:        payload.Input,
		Expected:     payload.Expected,
		Level:        Level4LLMJudge,
		Severity:     SeverityP0,
		BehaviorType: BehaviorSemanticQuality,
	}

	// 写入 validation 分区（隔离于 training），由人工审核后再迁移
	partition := "validation"
	if c.store != nil {
		if err := c.store.PutCase(ctx, partition, "incident", *evalCase); err != nil {
			return nil, fmt.Errorf("failed to save eval case: %w", err)
		}
	}

	return evalCase, nil
}
