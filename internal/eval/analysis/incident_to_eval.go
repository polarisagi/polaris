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
