package workflowadmin

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeHITL struct {
	resp *types.HITLResponse
	err  error
	got  []types.HITLPrompt
}

func (f *fakeHITL) Prompt(_ context.Context, p types.HITLPrompt) (*types.HITLResponse, error) {
	f.got = append(f.got, p)
	return f.resp, f.err
}

func TestHITLApprover(t *testing.T) {
	req := pipeline.ApprovalRequest{WorkflowID: "w", RunID: "r", StepKey: "s", Tool: "post", ArgsPreview: "{}"}
	cases := []struct {
		name    string
		gw      HITLPrompter
		wantErr bool
	}{
		{"nil gateway", nil, true},
		{"approved", &fakeHITL{resp: &types.HITLResponse{Approved: true}}, false},
		{"rejected", &fakeHITL{resp: &types.HITLResponse{Approved: false}}, true},
		{"nil resp", &fakeHITL{}, true},
		{"error", &fakeHITL{err: apperr.New(apperr.CodeInternal, "x")}, true},
	}
	for _, c := range cases {
		err := hitlApprover{gw: c.gw}.Approve(context.Background(), req)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

func TestPipelineLimitsFromConfig_PositiveDefaults(t *testing.T) {
	l := pipelineLimitsFromConfig()
	if l.MaxSteps <= 0 || l.MinIntervalMin <= 0 || l.LLMMaxInputChars <= 0 || l.LLMMaxOutputTokens <= 0 || l.StepDataMaxBytes <= 0 {
		t.Fatalf("limits must all be positive: %+v", l)
	}
	if k, d := pipelineRetentionFromConfig(); k <= 0 || d <= 0 {
		t.Fatalf("retention must be positive: %d %d", k, d)
	}
}
