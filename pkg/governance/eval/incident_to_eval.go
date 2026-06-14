package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type IncidentPayload struct {
	Input    map[string]any `json:"input"`
	Expected map[string]any `json:"expected"`
	Actual   map[string]any `json:"actual"`
	Details  string         `json:"details"`
}

type IncidentToEvalConverter struct {
	store *SQLiteEvalStore
}

func NewIncidentToEvalConverter(store *SQLiteEvalStore) *IncidentToEvalConverter {
	return &IncidentToEvalConverter{store: store}
}

func (c *IncidentToEvalConverter) Convert(ctx context.Context, incidentJSON []byte) (*EvalCase, error) {
	var payload IncidentPayload
	if err := json.Unmarshal(incidentJSON, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse incident payload: %w", err)
	}

	evalCase := &EvalCase{
		ID:           fmt.Sprintf("incident_%d", time.Now().UnixNano()),
		Name:         "Auto-generated from Incident",
		Description:  payload.Details,
		Input:        payload.Input,
		Expected:     payload.Expected,
		Level:        Level4LLMJudge,
		Severity:     SeverityP0,
		BehaviorType: BehaviorSemanticQuality,
	}

	if c.store != nil {
		if err := c.store.PutCase(ctx, "training", "incident", *evalCase); err != nil {
			return nil, fmt.Errorf("failed to save eval case: %w", err)
		}
	}

	return evalCase, nil
}
