package benchmark

import (
	"context"
	"encoding/json"
	"os"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// LoCoMoAdapter 实现 LoCoMo 长上下文记忆基准的转换。
type LoCoMoAdapter struct{}

func (a *LoCoMoAdapter) Name() string {
	return "locomo"
}

type locomoTask struct {
	ID             string `json:"id"`
	Context        string `json:"context"`
	Question       string `json:"question"`
	ExpectedAnswer string `json:"expected_answer"`
}

func (a *LoCoMoAdapter) Load(ctx context.Context, datasetPath string) ([]protocol.EvalCase, error) {
	data, err := os.ReadFile(datasetPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "LoCoMoAdapter: failed to read dataset", err)
	}

	var tasks []locomoTask
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "LoCoMoAdapter: failed to parse dataset", err)
	}

	var cases []protocol.EvalCase
	for _, t := range tasks {
		cases = append(cases, protocol.EvalCase{
			ID:                  t.ID,
			Input:               map[string]any{"context": t.Context, "query": t.Question},
			Expected:            map[string]any{"answer": t.ExpectedAnswer},
			BehaviorType:        protocol.BehaviorSemanticQuality,
			Level:               protocol.Level3Trajectory, // 根据长上下文推理要求定级
			FalsifiabilityScore: 1.0,
			Severity:            "P1",
			Source:              "locomo",
			Tags:                []string{"benchmark", "locomo", "long-context"},
		})
	}
	return cases, nil
}

// LongMemEvalAdapter 实现 LongMemEval 长时记忆基准的转换。
type LongMemEvalAdapter struct{}

func (a *LongMemEvalAdapter) Name() string {
	return "longmemeval"
}

type longMemTask struct {
	SessionID string   `json:"session_id"`
	History   []string `json:"history"`
	Query     string   `json:"query"`
	Gold      string   `json:"gold"`
}

func (a *LongMemEvalAdapter) Load(ctx context.Context, datasetPath string) ([]protocol.EvalCase, error) {
	data, err := os.ReadFile(datasetPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "LongMemEvalAdapter: failed to read dataset", err)
	}

	var tasks []longMemTask
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "LongMemEvalAdapter: failed to parse dataset", err)
	}

	var cases []protocol.EvalCase
	for _, t := range tasks {
		cases = append(cases, protocol.EvalCase{
			ID:                  t.SessionID,
			Input:               map[string]any{"history": t.History, "query": t.Query},
			Expected:            map[string]any{"answer": t.Gold},
			BehaviorType:        protocol.BehaviorSemanticQuality,
			Level:               protocol.Level3Trajectory,
			FalsifiabilityScore: 1.0,
			Severity:            "P1",
			Source:              "longmemeval",
			Tags:                []string{"benchmark", "longmemeval", "memory"},
		})
	}
	return cases, nil
}
