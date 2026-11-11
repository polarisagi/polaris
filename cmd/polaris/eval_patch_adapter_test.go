package main

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type recordingPatchProvider struct {
	protocol.Provider
	messages []types.Message
	options  types.InferOptions
	response *types.ProviderResponse
	err      error
}

func (p *recordingPatchProvider) Infer(_ context.Context, messages []types.Message, options ...types.InferOption) (*types.ProviderResponse, error) {
	p.messages = append([]types.Message(nil), messages...)
	for _, option := range options {
		option(&p.options)
	}
	return p.response, p.err
}

func (p *recordingPatchProvider) StreamInfer(context.Context, []types.Message, ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (p *recordingPatchProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (p *recordingPatchProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *recordingPatchProvider) ModelID() string                      { return "fixture-provider" }

func TestCodePatchCandidateAgent_OnlyProblemAndSourceReachProvider(t *testing.T) {
	provider := &recordingPatchProvider{response: &types.ProviderResponse{Content: "diff --git a/f.go b/f.go\n", Model: "fixture-model", Usage: types.Usage{InputTokens: 4, OutputTokens: 6}}}
	agent := newCodePatchCandidateAgent(provider)
	input := []byte(`{"problem_statement":"fix bug","source_files":{"f.go":"package f"}}`)
	output, tools, err := agent.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != provider.response.Content || len(tools) != 0 {
		t.Fatalf("unexpected candidate output %q %v", output, tools)
	}
	if len(provider.messages) != 1 {
		t.Fatalf("expected one user prompt, got %d", len(provider.messages))
	}
	if provider.options.Purpose != types.PurposeEvalCodePatch || provider.options.ThinkingMode != types.ThinkingMode("") || len(provider.options.Tools) != 0 {
		t.Fatalf("provider-only evaluation options are wrong: purpose=%s thinking=%s tools=%d", provider.options.Purpose, provider.options.ThinkingMode, len(provider.options.Tools))
	}
	stats := agent.CodePatchStats()
	if stats.Model != "fixture-model" || stats.Calls != 1 || stats.InputTokens != 4 || stats.OutputTokens != 6 || stats.CostAvailable || stats.ManualIntervention {
		t.Fatalf("provider usage metadata is incomplete: %+v", stats)
	}
	prompt := provider.messages[0].Content
	if !strings.Contains(prompt, "fix bug") || !strings.Contains(prompt, "f.go") {
		t.Fatalf("problem/source missing: %s", prompt)
	}
	for _, hidden := range []string{"test_patch", "FAIL_TO_PASS", "PASS_TO_PASS", "GOLD PATCH"} {
		if strings.Contains(prompt, hidden) {
			t.Fatalf("trusted eval data leaked to provider: %q", hidden)
		}
	}
}

func TestCodePatchCandidateAgent_RejectsEmptyAndToolCallResponses(t *testing.T) {
	for name, response := range map[string]*types.ProviderResponse{
		"empty":     {Content: "  "},
		"tool-call": {Content: "diff", ToolCalls: []types.InferToolCall{{Name: "run_command"}}},
	} {
		t.Run(name, func(t *testing.T) {
			provider := &recordingPatchProvider{response: response}
			agent := newCodePatchCandidateAgent(provider)
			input := []byte(`{"problem_statement":"fix bug","source_files":{"f.go":"package f"}}`)
			if _, _, err := agent.Run(context.Background(), input); err == nil {
				t.Fatal("invalid provider response must be rejected")
			}
		})
	}
}

func TestCodePatchCandidateAgent_RejectsTrustedTestConfig(t *testing.T) {
	provider := &recordingPatchProvider{}
	agent := newCodePatchCandidateAgent(provider)
	input := []byte(`{"problem_statement":"fix bug","source_files":{"f.go":"package f"},"test_patch":"secret"}`)
	if _, _, err := agent.Run(context.Background(), input); err == nil {
		t.Fatal("test configuration must be rejected before provider call")
	}
	if len(provider.messages) != 0 {
		t.Fatal("provider must not receive unsupported test config")
	}
}
