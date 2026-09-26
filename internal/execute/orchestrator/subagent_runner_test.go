package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type recordingHeadlessPool struct {
	mu       sync.Mutex
	queries  []string
	profiles []*types.AgentProfileSpec
}

func (p *recordingHeadlessPool) Acquire(context.Context, string) (protocol.AgentController, func(), error) {
	return nil, func() {}, nil
}

func (p *recordingHeadlessPool) AcquireHeadless(_ context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	o := &types.HeadlessOptions{}
	for _, fn := range opts {
		fn(o)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries = append(p.queries, intent.Query)
	p.profiles = append(p.profiles, o.Profile)
	return &types.AgentResult{Output: "answer"}, nil
}

type scriptedSubagentHooks struct {
	startCtx    string
	stopReasons int // 要求继续的次数；-1 = 始终要求继续
	starts      []string
	stops       []bool
}

func (h *scriptedSubagentHooks) FireSubagentStart(_ context.Context, sessionID, _, agentType string) string {
	h.starts = append(h.starts, sessionID+"/"+agentType)
	return h.startCtx
}

func (h *scriptedSubagentHooks) FireSubagentStop(_ context.Context, _, _, _, _ string, active bool) string {
	h.stops = append(h.stops, active)
	if h.stopReasons < 0 || len(h.stops) <= h.stopReasons {
		return "run the tests too"
	}
	return ""
}

func TestSubagentRunner_HooksAndBoundedContinuation(t *testing.T) {
	pool := &recordingHeadlessPool{}
	hooks := &scriptedSubagentHooks{startCtx: "repo uses pnpm", stopReasons: -1}
	spec := &types.AgentProfileSpec{Name: "review:security"}
	r := NewSubagentRunner(pool, mapProfileResolver{"review:security": spec}, hooks)

	out, err := r.Run(context.Background(), SubagentRequest{ParentSessionID: "s1", AgentName: "review:security", Prompt: "audit"})
	if err != nil || out != "answer" {
		t.Fatalf("run: %q %v", out, err)
	}
	if len(hooks.starts) != 1 || hooks.starts[0] != "s1/review:security" || !strings.Contains(pool.queries[0], "<hook-context>\nrepo uses pnpm") {
		t.Fatalf("SubagentStart context must reach the subagent: %v %q", hooks.starts, pool.queries[0])
	}
	if len(pool.queries) != 1+maxSubagentStopContinuations || len(hooks.stops) != maxSubagentStopContinuations ||
		hooks.stops[0] || !hooks.stops[1] {
		t.Fatalf("continuations must be bounded with stop_hook_active: queries=%d stops=%v", len(pool.queries), hooks.stops)
	}
	if !strings.Contains(pool.queries[1], "<previous_answer>\nanswer") || pool.profiles[1] != spec {
		t.Fatalf("continuation must carry the task, previous answer and profile: %q", pool.queries[1])
	}
}

func TestSubagentRunner_SuppressedProfileSkipsHooks(t *testing.T) {
	pool := &recordingHeadlessPool{}
	hooks := &scriptedSubagentHooks{stopReasons: -1}
	r := NewSubagentRunner(pool, nil, hooks)
	if _, err := r.Run(context.Background(), SubagentRequest{Profile: &types.AgentProfileSpec{Name: "hook-agent", SuppressHooks: true}, Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(hooks.starts)+len(hooks.stops) != 0 || len(pool.queries) != 1 {
		t.Fatalf("hook-agent runs must not fire subagent hooks: %v %v", hooks.starts, hooks.stops)
	}
}
