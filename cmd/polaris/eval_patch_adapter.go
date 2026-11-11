package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/prompt/templates"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type codePatchCandidateAgent struct {
	provider protocol.Provider
	mu       sync.Mutex
	stats    types.CodePatchEvalMetadata
}

type codePatchCandidateInput struct {
	ProblemStatement string            `json:"problem_statement"`
	SourceFiles      map[string]string `json:"source_files"`
}

func newCodePatchCandidateAgent(provider protocol.Provider) *codePatchCandidateAgent {
	return &codePatchCandidateAgent{provider: provider}
}

func (a *codePatchCandidateAgent) Run(ctx context.Context, input []byte) ([]byte, []string, error) {
	if a == nil || a.provider == nil {
		return nil, nil, apperr.New(apperr.CodeServiceUnavailable, "code patch candidate provider is unavailable")
	}
	var candidateInput codePatchCandidateInput
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidateInput); err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInvalidInput, "code patch candidate: unsupported input", err)
	}
	if strings.TrimSpace(candidateInput.ProblemStatement) == "" || len(candidateInput.SourceFiles) == 0 {
		return nil, nil, apperr.New(apperr.CodeInvalidInput, "code patch candidate: problem statement and source files required")
	}
	userPrompt, err := templates.Render("code_patch_candidate.tmpl", candidateInput)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "code patch candidate: render prompt", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	started := time.Now()
	response, err := safecall.Infer(callCtx, a.provider, []types.Message{{Role: "user", Content: userPrompt}}, types.WithThinkingMode(types.ThinkingMode("")), types.WithPurpose(types.PurposeEvalCodePatch))
	model := a.provider.ModelID()
	usage := types.Usage{}
	if response != nil {
		if response.Model != "" {
			model = response.Model
		}
		usage = response.Usage
	}
	a.record(model, time.Since(started), usage)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeProviderExhausted, "code patch candidate: provider inference failed", err)
	}
	if response == nil || len(response.ToolCalls) > 0 || strings.TrimSpace(response.Content) == "" {
		return nil, nil, apperr.New(apperr.CodeInternal, "code patch candidate: invalid provider response")
	}
	return []byte(response.Content), nil, nil
}

func (a *codePatchCandidateAgent) record(model string, duration time.Duration, usage types.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stats.Calls == 0 {
		a.stats.Model = model
	} else if model != "" && a.stats.Model != model {
		a.stats.Model = "mixed"
	}
	a.stats.Calls++
	a.stats.DurationMs += duration.Milliseconds()
	a.stats.InputTokens += usage.InputTokens
	a.stats.OutputTokens += usage.OutputTokens
	// ProviderResponse does not carry a currency-normalized cost field.
	a.stats.CostAvailable = false
	a.stats.ManualIntervention = false
}

func (a *codePatchCandidateAgent) CodePatchStats() types.CodePatchEvalMetadata {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

func (a *codePatchCandidateAgent) ResetCodePatchStats() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stats = types.CodePatchEvalMetadata{}
}

var _ interface {
	Run(context.Context, []byte) ([]byte, []string, error)
} = (*codePatchCandidateAgent)(nil)
