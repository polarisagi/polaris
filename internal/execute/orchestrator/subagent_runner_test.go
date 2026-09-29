package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
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

type fakeRunRecorder struct {
	mu       sync.Mutex
	started  []repo.SubagentRunRow
	finished []fakeFinish
	startErr error
}

type fakeFinish struct {
	id, status, output, errMsg string
	continuations              int
}

func (f *fakeRunRecorder) Start(_ context.Context, row repo.SubagentRunRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, row)
	return f.startErr
}

func (f *fakeRunRecorder) Finish(_ context.Context, id, status, output, errMsg string, continuations int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, fakeFinish{id, status, output, errMsg, continuations})
	return nil
}

// sessionCapturePool 记录 AcquireHeadless 实际收到的 SessionID。
type sessionCapturePool struct {
	recordingHeadlessPool
	sessions []string
	err      error
}

func (p *sessionCapturePool) AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	o := &types.HeadlessOptions{}
	for _, fn := range opts {
		fn(o)
	}
	p.sessions = append(p.sessions, o.SessionID)
	if p.err != nil {
		return nil, p.err
	}
	return p.recordingHeadlessPool.AcquireHeadless(ctx, intent, opts...)
}

func TestSubagentRunner_RecordsRunAndDeterministicSession(t *testing.T) {
	pool := &sessionCapturePool{}
	rec := &fakeRunRecorder{}
	hooks := &scriptedSubagentHooks{stopReasons: 1}
	r := NewSubagentRunner(pool, nil, hooks).WithRecorder(rec)

	long := strings.Repeat("字", subagentPromptMaxRunes+10)
	if _, err := r.Run(context.Background(), SubagentRequest{ParentSessionID: "p1", AgentID: "ag1", Prompt: long,
		Entry: SubagentEntryDelegation, TaskID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.started) != 1 || rec.started[0].ID != "ag1" || rec.started[0].ChildSessionID != "sub-ag1" ||
		rec.started[0].TaskID != "t1" || rec.started[0].Entry != "delegation" || rec.started[0].ParentSessionID != "p1" {
		t.Fatalf("start row: %+v", rec.started)
	}
	if got := len([]rune(rec.started[0].Prompt)); got != subagentPromptMaxRunes {
		t.Errorf("prompt 应截断到 %d 字符, got %d", subagentPromptMaxRunes, got)
	}
	if pool.sessions[0] != "sub-ag1" {
		t.Errorf("子 Agent 应使用确定会话 ID, got %q", pool.sessions[0])
	}
	if len(rec.finished) != 1 || rec.finished[0].status != "ok" || rec.finished[0].continuations != 1 || rec.finished[0].output != "answer" {
		t.Fatalf("finish: %+v", rec.finished)
	}
}

func TestSubagentRunner_CallerSessionIDWins(t *testing.T) {
	pool := &sessionCapturePool{}
	rec := &fakeRunRecorder{}
	r := NewSubagentRunner(pool, nil, nil).WithRecorder(rec)
	_, err := r.Run(context.Background(), SubagentRequest{AgentID: "ag2", Prompt: "x", Entry: SubagentEntryHook,
		Options: []types.HeadlessOption{types.WithSessionID("custom")}})
	if err != nil {
		t.Fatal(err)
	}
	if pool.sessions[0] != "custom" || rec.started[0].ChildSessionID != "custom" {
		t.Errorf("调用方 SessionID 应胜出: pool=%q rec=%q", pool.sessions[0], rec.started[0].ChildSessionID)
	}
}

func TestSubagentRunner_RecordsErrorAndSurvivesRecorderFailure(t *testing.T) {
	pool := &sessionCapturePool{err: apperr.New(apperr.CodeInternal, "boom")}
	rec := &fakeRunRecorder{startErr: apperr.New(apperr.CodeInternal, "db down")}
	r := NewSubagentRunner(pool, nil, nil).WithRecorder(rec)
	_, err := r.Run(context.Background(), SubagentRequest{AgentID: "ag3", Prompt: "x", Entry: SubagentEntryForkSkill})
	if err == nil {
		t.Fatal("子 Agent 失败应原样返回错误")
	}
	if len(rec.finished) != 1 || rec.finished[0].status != "error" || rec.finished[0].errMsg == "" {
		t.Fatalf("finish: %+v", rec.finished)
	}
}

func TestSubagentRunner_RunSubagentUsesForkSkillEntry(t *testing.T) {
	rec := &fakeRunRecorder{}
	r := NewSubagentRunner(&sessionCapturePool{}, nil, nil).WithRecorder(rec)
	if _, err := r.RunSubagent(context.Background(), "s1", "", "do"); err != nil {
		t.Fatal(err)
	}
	if rec.started[0].Entry != SubagentEntryForkSkill {
		t.Errorf("entry: %s", rec.started[0].Entry)
	}
}
