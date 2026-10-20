package session

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

type fakeTurnStore struct {
	mu    sync.Mutex
	recs  []StepRecord
	usage TurnUsageSummary
	meta  map[int64]string
}

func (f *fakeTurnStore) AppendTurnSteps(_ context.Context, _, _ string, recs []StepRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, recs...)
	return nil
}

func (f *fakeTurnStore) TurnUsage(context.Context, string) (TurnUsageSummary, error) {
	return f.usage, nil
}

func (f *fakeTurnStore) AttachTurnMeta(_ context.Context, id int64, _ string, usage string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.meta == nil {
		f.meta = map[int64]string{}
	}
	f.meta[id] = usage
	return nil
}

func (f *fakeTurnStore) snapshot() []StepRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StepRecord(nil), f.recs...)
}

// stepsOf 取 sink 中某 phase 的 step 载荷。
func stepsOf(s *recordingSink, phase string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, e := range s.events {
		if e.Kind == KindStep && e.Payload["phase"] == phase {
			out = append(out, e.Payload)
		}
	}
	return out
}

func tc(name, call string) types.AgentStreamEvent {
	return types.AgentStreamEvent{Type: types.AgentStreamEventToolCall, ToolName: name, CallID: call, ToolInput: []byte(`{"q":"x"}`)}
}

func tr(name, call, raw string, isErr bool) types.AgentStreamEvent {
	return types.AgentStreamEvent{Type: types.AgentStreamEventToolResult, ToolName: name, CallID: call, ResultRaw: raw, IsError: isErr, DurationMs: 5}
}

func TestStepRecorder_ParallelToolsPairByCallID(t *testing.T) {
	sink := &recordingSink{}
	r := newStepRecorder(context.Background(), sink, nil, "s", "turn_a")
	r.onEvent(tc("run_command", "c1"))
	r.onEvent(tc("run_command", "c2"))
	// 结果乱序返回：c2 先到且失败。
	r.onEvent(tr("run_command", "c2", `{"command":"b","output":"boom","exit_code":2}`, true))
	r.onEvent(tr("run_command", "c1", `{"command":"a","output":"ok","exit_code":0}`, false))
	sum := r.finalize(false)

	ends := stepsOf(sink, stepPhaseEnd)
	if len(ends) != 2 {
		t.Fatalf("end events = %d, want 2", len(ends))
	}
	if ends[0]["id"] != "tool-c2" || ends[0]["status"] != StepStatusError {
		t.Errorf("first end = %v, want tool-c2 error", ends[0])
	}
	if ends[1]["id"] != "tool-c1" || ends[1]["status"] != StepStatusOK {
		t.Errorf("second end = %v, want tool-c1 ok", ends[1])
	}
	if sum.Tools != 2 || sum.Total != 2 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestStepRecorder_ThinkingLifecycleAndTruncation(t *testing.T) {
	sink := &recordingSink{}
	st := &fakeTurnStore{}
	r := newStepRecorder(context.Background(), sink, st, "s", "turn_a")
	big := strings.Repeat("思", 6000) // 18KB > 8KB
	r.onEvent(types.AgentStreamEvent{Type: types.AgentStreamEventThinking, Content: big})
	r.onEvent(types.AgentStreamEvent{Type: types.AgentStreamEventToken, Content: "hi"}) // 关闭思考
	r.finalize(false)

	if got := len(stepsOf(sink, stepPhaseStart)); got != 1 {
		t.Fatalf("start events = %d, want 1", got)
	}
	ends := stepsOf(sink, stepPhaseEnd)
	if len(ends) != 1 || ends[0]["kind"] != StepKindThinking {
		t.Fatalf("ends = %v", ends)
	}
	if c, _ := ends[0]["content"].(string); len(c) > thinkingPersistMax+64 || len(c) == 0 {
		t.Errorf("content bytes = %d, want 0 < n <= %d", len(c), thinkingPersistMax)
	}
	var persisted bool
	for _, rec := range st.snapshot() {
		if rec.Kind == StepKindThinking {
			persisted = true
			var s Step
			_ = json.Unmarshal(rec.Data, &s)
			if len(s.Content) > thinkingPersistMax+64 {
				t.Errorf("persisted content %d bytes exceeds cap", len(s.Content))
			}
		}
	}
	if !persisted {
		t.Error("thinking step not persisted")
	}
}

func TestStepRecorder_SubagentNesting(t *testing.T) {
	sink := &recordingSink{}
	r := newStepRecorder(context.Background(), sink, nil, "s", "turn_a")
	ev := tc("web_search", "k1")
	ev.AgentID, ev.ParentID, ev.IsNested, ev.ChildAgentRole = "ag1", "root", true, "researcher"
	r.onEvent(ev)
	res := tr("web_search", "k1", `{"engine":"ddg","results":[{"title":"A","url":"u","snippet":"s"}]}`, false)
	res.AgentID, res.ParentID, res.IsNested = "ag1", "root", true
	r.onEvent(res)
	r.finalize(false)

	var sub, tool map[string]any
	for _, p := range stepsOf(sink, stepPhaseEnd) {
		switch p["kind"] {
		case StepKindSubagent:
			sub = p
		case StepKindTool:
			tool = p
		}
	}
	if sub == nil || tool == nil {
		t.Fatalf("missing subagent/tool end events: sub=%v tool=%v", sub, tool)
	}
	if tool["parent_id"] != sub["id"] {
		t.Errorf("tool parent = %v, want %v", tool["parent_id"], sub["id"])
	}
}

func TestStepRecorder_ApprovalClosedByResult(t *testing.T) {
	for _, tc2 := range []struct {
		name, raw, want string
		isErr           bool
	}{
		{"approved", `{"output":"fine"}`, StepStatusOK, false},
		{"denied", "Error: user denied the operation", StepStatusDenied, true},
	} {
		t.Run(tc2.name, func(t *testing.T) {
			sink := &recordingSink{}
			r := newStepRecorder(context.Background(), sink, nil, "s", "turn_a")
			r.onEvent(types.AgentStreamEvent{Type: types.AgentStreamEventApproval, ToolName: "run_command", ToolInput: []byte(`{"command":"rm"}`)})
			r.onEvent(tc("run_command", "c1"))
			r.onEvent(tr("run_command", "c1", tc2.raw, tc2.isErr))
			r.finalize(false)
			for _, p := range stepsOf(sink, stepPhaseEnd) {
				if p["kind"] == StepKindApproval && p["status"] != tc2.want {
					t.Errorf("approval status = %v, want %s", p["status"], tc2.want)
				}
			}
		})
	}
}

func TestStepRecorder_LLMAndRetry(t *testing.T) {
	sink := &recordingSink{}
	r := newStepRecorder(context.Background(), sink, nil, "s", "turn_a")
	r.onEvent(types.AgentStreamEvent{Type: types.AgentStreamEventLLM, ToolName: "chat", InTokens: 100, OutTokens: 20, CacheHitTokens: 50, DurationMs: 300, Retry: true})
	sum := r.finalize(false)
	kinds := map[string]int{}
	for _, p := range stepsOf(sink, stepPhaseEnd) {
		kinds[p["kind"].(string)]++
	}
	if kinds[StepKindRetry] != 1 || kinds[StepKindLLM] != 1 {
		t.Errorf("kinds = %v", kinds)
	}
	if sum.Tokens.In != 100 || sum.Tokens.Out != 20 || sum.Tokens.CacheHit != 50 {
		t.Errorf("tokens = %+v", sum.Tokens)
	}
}

func TestStepRecorder_AbortedMarksRunningCancelled(t *testing.T) {
	sink := &recordingSink{}
	r := newStepRecorder(context.Background(), sink, nil, "s", "turn_a")
	r.onEvent(tc("web_fetch", "c1"))
	r.finalize(true)
	ends := stepsOf(sink, stepPhaseEnd)
	if len(ends) != 1 || ends[0]["status"] != StepStatusCancelled {
		t.Errorf("ends = %v, want one cancelled", ends)
	}
}

func TestStepRecorder_OverflowKeepsHeadTailAndSummary(t *testing.T) {
	sink := &recordingSink{}
	st := &fakeTurnStore{}
	r := newStepRecorder(context.Background(), sink, st, "s", "turn_a")
	const n = 500
	for i := 0; i < n; i++ {
		r.onEvent(types.AgentStreamEvent{Type: types.AgentStreamEventNotice, Content: "n"})
	}
	sum := r.finalize(false)
	recs := st.snapshot()
	if len(recs) > 300 {
		t.Errorf("persisted rows = %d, want <= 300", len(recs))
	}
	var bytes int
	var gotSummary bool
	for _, rec := range recs {
		bytes += len(rec.Data)
		if rec.Seq == 0 && rec.Kind == stepKindSummary {
			gotSummary = true
		}
	}
	if bytes > 256<<10 {
		t.Errorf("persisted bytes = %d, want <= 256KB", bytes)
	}
	if !gotSummary {
		t.Error("summary row missing")
	}
	if sum.Total != n || sum.Omitted != n-maxPersistHeadSteps-maxPersistTailSteps {
		t.Errorf("summary total=%d omitted=%d", sum.Total, sum.Omitted)
	}
	// 头部第一条与尾部最后一条都在。
	var hasFirst, hasLast bool
	for _, rec := range recs {
		hasFirst = hasFirst || rec.Seq == 1
		hasLast = hasLast || rec.Seq == n
	}
	if !hasFirst || !hasLast {
		t.Errorf("hasFirst=%v hasLast=%v", hasFirst, hasLast)
	}
}

func TestRunTurn_Interactive_EmitsStepsAndCompleteMeta(t *testing.T) {
	ctrl := newFakeAgentController()
	go func() {
		ctrl.events <- tc("web_search", "c1")
		ctrl.events <- tr("web_search", "c1", `{"engine":"ddg","results":[{"title":"A","url":"u","snippet":"s"}]}`, false)
		ctrl.events <- types.AgentStreamEvent{Type: types.AgentStreamEventToken, Content: "done"}
		ctrl.events <- types.AgentStreamEvent{Type: types.AgentStreamEventStatus, Content: "task_done"}
	}()
	persistence := newFakePersistence()
	pool := &fakeAgentPool{ctrl: ctrl}
	orc := newTestOrchestrator(t, persistence, &fakeHooks{}, &fakeSlash{}, &fakeCompression{}, pool)
	sink := &recordingSink{}
	if _, err := orc.RunTurn(context.Background(), Request{SessionID: "s1", Input: "hi", Channel: "web"}, sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !sink.hasKind(KindStep) {
		t.Fatal("expected KindStep events")
	}
	var complete map[string]any
	sink.mu.Lock()
	for _, e := range sink.events {
		if e.Kind == KindComplete {
			complete = e.Payload
		}
	}
	sink.mu.Unlock()
	if complete == nil {
		t.Fatal("no complete event")
	}
	if id, _ := complete["turn_id"].(string); id == "" {
		t.Errorf("complete payload lacks turn_id: %v", complete)
	}
}
