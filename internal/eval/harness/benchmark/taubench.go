package benchmark

import (
	"context"
	"encoding/json"
	"os"

	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type TauBenchAdapter struct{}

func (a *TauBenchAdapter) Name() string {
	return "tau-bench"
}

// taubenchTask represents a single task in tau-bench.
type taubenchTask struct {
	TaskID         string           `json:"task_id"`
	UserGoal       string           `json:"user_goal"`
	AvailableTools []string         `json:"available_tools"`
	GoldenActions  []map[string]any `json:"golden_actions"`
}

func (a *TauBenchAdapter) Load(ctx context.Context, datasetPath string) ([]harness.EvalCase, error) {
	data, err := os.ReadFile(datasetPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "TauBenchAdapter: failed to read dataset", err)
	}

	var tasks []taubenchTask
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "TauBenchAdapter: failed to parse dataset", err)
	}

	var cases []harness.EvalCase
	for _, t := range tasks {
		cases = append(cases, harness.EvalCase{
			ID:                  t.TaskID,
			Input:               map[string]any{"query": t.UserGoal, "tools": t.AvailableTools},
			Expected:            map[string]any{"tool_calls": t.GoldenActions},
			BehaviorType:        harness.BehaviorToolCallSequence,
			Level:               harness.Level3Trajectory,
			FalsifiabilityScore: 1.0,
			Severity:            "P1",
			Source:              "tau-bench",
			Tags:                []string{"benchmark", "tau-bench"},
		})
	}
	return cases, nil
}
