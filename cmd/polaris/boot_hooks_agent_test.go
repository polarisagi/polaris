package main

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type profileCapturingPool struct{ profile *types.AgentProfileSpec }

func (p *profileCapturingPool) Acquire(context.Context, string) (protocol.AgentController, func(), error) {
	return nil, func() {}, nil
}

func (p *profileCapturingPool) AcquireHeadless(_ context.Context, _ types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	o := &types.HeadlessOptions{}
	for _, fn := range opts {
		fn(o)
	}
	p.profile = o.Profile
	return &types.AgentResult{Output: `{"ok":true}`}, nil
}

func TestHookPromptEvaluator_AgentTypeRunsReadOnlySubagent(t *testing.T) {
	e := &hookPromptEvaluator{}
	if _, err := e.EvaluateHookPrompt(context.Background(), "check", "", true); err == nil {
		t.Fatal("agent hooks must fail until the subagent runner is bound")
	}
	pool := &profileCapturingPool{}
	e.bindSubagents(orchestrator.NewSubagentRunner(pool, nil, nil))
	out, err := e.EvaluateHookPrompt(context.Background(), "check", "", true)
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("agent hook: %q %v", out, err)
	}
	if p := pool.profile; p == nil || !p.ReadOnly || !p.SuppressHooks || p.MaxTurns != 50 || p.AllowDelegation {
		t.Fatalf("hook agent must be read-only, hook-suppressed and bounded: %+v", p)
	}
}
