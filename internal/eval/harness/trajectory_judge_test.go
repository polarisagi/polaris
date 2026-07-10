package harness

import (
	"context"
	"testing"
)

func TestTrajectoryJudge_Evaluate(t *testing.T) {
	judge := NewTrajectoryJudge()
	ctx := context.Background()

	tests := []struct {
		name      string
		trace     *TrajectoryTrace
		rules     *TrajectoryRules
		wantPass  bool
		wantError string
	}{
		{
			name:      "nil trace",
			trace:     nil,
			rules:     &TrajectoryRules{},
			wantPass:  false,
			wantError: "trace is nil",
		},
		{
			name:      "nil rules",
			trace:     &TrajectoryTrace{},
			rules:     nil,
			wantPass:  true,
			wantError: "",
		},
		{
			name: "max llm calls exceeded",
			trace: &TrajectoryTrace{
				LLMCalls: []LLMCallRecord{{}, {}, {}},
			},
			rules: &TrajectoryRules{
				MaxLLMCalls: 2,
			},
			wantPass:  false,
			wantError: "llm calls exceeded max limit: 3 > 2",
		},
		{
			name: "max tool calls exceeded",
			trace: &TrajectoryTrace{
				ToolCalls: []ToolCallRecord{{}, {}},
			},
			rules: &TrajectoryRules{
				MaxToolCalls: 1,
			},
			wantPass:  false,
			wantError: "tool calls exceeded max limit: 2 > 1",
		},
		{
			name: "used prohibited tool",
			trace: &TrajectoryTrace{
				ToolCalls: []ToolCallRecord{
					{Name: "safe_tool"},
					{Name: "dangerous_tool"},
				},
			},
			rules: &TrajectoryRules{
				ProhibitedTools: []string{"dangerous_tool"},
			},
			wantPass:  false,
			wantError: "used prohibited tool: dangerous_tool",
		},
		{
			name: "valid sequence",
			trace: &TrajectoryTrace{
				ToolCalls: []ToolCallRecord{
					{Name: "search"},
					{Name: "read"},
					{Name: "write"},
				},
			},
			rules: &TrajectoryRules{
				ExpectedToolSequence: []string{"search", "write"},
			},
			wantPass:  true,
			wantError: "",
		},
		{
			name: "missing from sequence",
			trace: &TrajectoryTrace{
				ToolCalls: []ToolCallRecord{
					{Name: "search"},
					{Name: "read"},
				},
			},
			rules: &TrajectoryRules{
				ExpectedToolSequence: []string{"search", "write"},
			},
			wantPass:  false,
			wantError: "missing expected tool sequence or out of order: write",
		},
		{
			name: "out of order sequence",
			trace: &TrajectoryTrace{
				ToolCalls: []ToolCallRecord{
					{Name: "write"},
					{Name: "search"},
				},
			},
			rules: &TrajectoryRules{
				ExpectedToolSequence: []string{"search", "write"},
			},
			wantPass:  false,
			wantError: "missing expected tool sequence or out of order: write",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPass, gotErr := judge.Evaluate(ctx, tt.trace, tt.rules)
			if gotPass != tt.wantPass {
				t.Errorf("Evaluate() gotPass = %v, want %v", gotPass, tt.wantPass)
			}
			if gotErr != tt.wantError {
				t.Errorf("Evaluate() gotErr = %v, want %v", gotErr, tt.wantError)
			}
		})
	}
}
