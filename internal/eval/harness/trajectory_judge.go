package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// TrajectoryRules defines the expectations for a valid Agent trajectory.
type TrajectoryRules struct {
	MaxLLMCalls          int      `json:"max_llm_calls,omitempty"`
	MaxToolCalls         int      `json:"max_tool_calls,omitempty"`
	ProhibitedTools      []string `json:"prohibited_tools,omitempty"`
	ExpectedToolSequence []string `json:"expected_tool_sequence,omitempty"`
}

// TrajectoryJudge evaluates a TrajectoryTrace against TrajectoryRules.
type TrajectoryJudge struct{}

func NewTrajectoryJudge() *TrajectoryJudge {
	return &TrajectoryJudge{}
}

// Evaluate checks if the given trace complies with the rules.
// Returns (passed bool, errorMsg string).
//
//nolint:gocyclo
func (j *TrajectoryJudge) Evaluate(ctx context.Context, trace *TrajectoryTrace, rules *TrajectoryRules) (bool, string) {
	if trace == nil {
		return false, "trace is nil"
	}
	if rules == nil {
		return true, "" // no rules to enforce
	}

	if rules.MaxLLMCalls > 0 && len(trace.LLMCalls) > rules.MaxLLMCalls {
		return false, fmt.Sprintf("llm calls exceeded max limit: %d > %d", len(trace.LLMCalls), rules.MaxLLMCalls)
	}

	if rules.MaxToolCalls > 0 && len(trace.ToolCalls) > rules.MaxToolCalls {
		return false, fmt.Sprintf("tool calls exceeded max limit: %d > %d", len(trace.ToolCalls), rules.MaxToolCalls)
	}

	// Check prohibited tools
	if len(rules.ProhibitedTools) > 0 {
		prohibitedMap := make(map[string]bool, len(rules.ProhibitedTools))
		for _, pt := range rules.ProhibitedTools {
			prohibitedMap[pt] = true
		}

		for _, tc := range trace.ToolCalls {
			if prohibitedMap[tc.Name] {
				return false, fmt.Sprintf("used prohibited tool: %s", tc.Name)
			}
		}
	}

	// Check expected tool sequence
	// We require that the expected tools appear in the exact relative order within the trace.
	if len(rules.ExpectedToolSequence) > 0 {
		seqIdx := 0
		for _, tc := range trace.ToolCalls {
			if seqIdx < len(rules.ExpectedToolSequence) && tc.Name == rules.ExpectedToolSequence[seqIdx] {
				seqIdx++
			}
		}
		if seqIdx < len(rules.ExpectedToolSequence) {
			missing := strings.Join(rules.ExpectedToolSequence[seqIdx:], ", ")
			return false, fmt.Sprintf("missing expected tool sequence or out of order: %s", missing)
		}
	}

	return true, ""
}

// ParseTrace extracts a TrajectoryTrace from map[string]any.
func ParseTrace(input map[string]any) (*TrajectoryTrace, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to marshal trace", err)
	}
	var trace TrajectoryTrace
	if err := json.Unmarshal(b, &trace); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to unmarshal trace", err)
	}
	return &trace, nil
}

// ParseRules extracts TrajectoryRules from map[string]any.
func ParseRules(expected map[string]any) (*TrajectoryRules, error) {
	b, err := json.Marshal(expected)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to marshal rules", err)
	}
	var rules TrajectoryRules
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to unmarshal rules", err)
	}
	return &rules, nil
}
