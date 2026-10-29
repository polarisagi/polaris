package sleeptime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/learning/sleeptime"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockInferCaller struct {
	lastReq  types.InferRequest
	lastOpts types.InferOptions
	respText string
	err      error
}

func (m *mockInferCaller) CallInfer(ctx context.Context, req types.InferRequest, opts types.InferOptions) (types.InferResponse, error) {
	m.lastReq = req
	m.lastOpts = opts
	if m.err != nil {
		return types.InferResponse{}, m.err
	}
	return types.InferResponse{Content: m.respText}, nil
}

type mockValidator struct {
	invalidPrefix string
}

func (v *mockValidator) ValidatePath(relPath string) error {
	if v.invalidPrefix != "" && strings.HasPrefix(relPath, v.invalidPrefix) {
		return apperr.New(apperr.CodeForbidden, "invalid path")
	}
	return nil
}

func TestDefaultProposer_Propose(t *testing.T) {
	ctx := context.Background()
	jsonResp := "```json\n" + `[
		{"path": "tool_notes/bash.md", "content": "Avoid long sleeps.", "reason": "Timeout prevention"},
		{"path": "forbidden/secret.txt", "content": "Evil", "reason": "Attack"},
		{"path": "prompt_addenda/rules.md", "content": "Always verify inputs.", "reason": "Input validation"}
	]` + "\n```"

	caller := &mockInferCaller{respText: jsonResp}
	validator := &mockValidator{invalidPrefix: "forbidden/"}

	proposer := sleeptime.NewDefaultProposer(caller, validator)

	weaknesses := []sleeptime.Weakness{
		{Signature: "agentic_loop:timeout", Count: 5, SampleErrors: []string{"timed out"}},
	}

	proposals, err := proposer.Propose(ctx, weaknesses)
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}

	// 验证 T8 模型池与思考档位设置
	if caller.lastOpts.Purpose != "self_harness_propose" {
		t.Errorf("expected purpose 'self_harness_propose', got %q", caller.lastOpts.Purpose)
	}
	if caller.lastOpts.ModelPool != "default" {
		t.Errorf("expected model pool 'default', got %q", caller.lastOpts.ModelPool)
	}
	if caller.lastOpts.ThinkingMode != types.ThinkingDisabled {
		t.Errorf("expected thinking disabled, got %v", caller.lastOpts.ThinkingMode)
	}

	// 验证非法路径被过滤，合法路径被保留
	if len(proposals) != 2 {
		t.Fatalf("expected 2 valid proposals, got %d", len(proposals))
	}
	if proposals[0].Path != "tool_notes/bash.md" {
		t.Errorf("unexpected path: %s", proposals[0].Path)
	}
	if proposals[1].Path != "prompt_addenda/rules.md" {
		t.Errorf("unexpected path: %s", proposals[1].Path)
	}
}
