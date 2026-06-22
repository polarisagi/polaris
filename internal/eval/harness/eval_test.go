package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestRegressionDetector_Check(t *testing.T) {
	rd := &RegressionDetector{}

	baseline := &RunMetrics{TaskSuccessRate: 0.90, AvgLatencyMs: 1000, TokenBurnRate: 1000}
	current := &RunMetrics{TaskSuccessRate: 0.80, AvgLatencyMs: 1300, TokenBurnRate: 1400}

	// TaskSuccessRate trigger
	alert := rd.Check(baseline, current)
	if alert == nil {
		t.Fatalf("expected alert for task success rate drop")
	}
	if alert.Metric != "task_success_rate" {
		t.Errorf("expected metric task_success_rate, got %s", alert.Metric)
	}

	// Latency trigger
	current.TaskSuccessRate = 0.95
	alert = rd.Check(baseline, current)
	if alert == nil {
		t.Fatalf("expected alert for latency rise")
	}
	if alert.Metric != "avg_latency_ms" {
		t.Errorf("expected metric avg_latency_ms, got %s", alert.Metric)
	}

	// TokenBurnRate trigger
	current.AvgLatencyMs = 900
	alert = rd.Check(baseline, current)
	if alert == nil {
		t.Fatalf("expected alert for token burn rate rise")
	}
	if alert.Metric != "token_burn_rate" {
		t.Errorf("expected metric token_burn_rate, got %s", alert.Metric)
	}

	// No alert
	current.TokenBurnRate = 1000
	alert = rd.Check(baseline, current)
	if alert != nil {
		t.Fatalf("expected no alert, got %v", alert)
	}
}

type mockStore struct {
	protocol.Store
	values [][]byte
}

func (m *mockStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	return &mockIterator{values: m.values}, nil
}

type mockIterator struct {
	values [][]byte
	idx    int
}

func (m *mockIterator) Next() bool {
	if m.idx < len(m.values) {
		m.idx++
		return true
	}
	return false
}
func (m *mockIterator) Key() []byte   { return nil }
func (m *mockIterator) Value() []byte { return m.values[m.idx-1] }
func (m *mockIterator) Err() error    { return nil }
func (m *mockIterator) Close() error  { return nil }
func (m *mockIterator) Seek([]byte)   {}

func TestTrajectoryRecorder(t *testing.T) {
	v1, _ := json.Marshal(map[string]any{"type": "llm_call", "request": map[string]any{"a": "b"}, "response": map[string]any{"c": "d"}})
	v2, _ := json.Marshal(map[string]any{"type": "tool_call", "tool": "test", "args": map[string]any{}, "result": map[string]any{}})
	v3, _ := json.Marshal(map[string]any{"type": "state_1"})
	v4, _ := json.Marshal(map[string]any{"type": "state_2"})

	ms := &mockStore{values: [][]byte{v1, v2, v3, v4}}
	recorder := NewTrajectoryRecorder(ms)

	trace, err := recorder.Record(context.Background(), "session1")
	if err != nil {
		t.Fatal(err)
	}

	if trace.SessionID != "session1" {
		t.Errorf("expected session1")
	}
	if len(trace.LLMCalls) != 1 {
		t.Errorf("expected 1 LLM call")
	}
	if len(trace.ToolCalls) != 1 {
		t.Errorf("expected 1 Tool call")
	}
	if len(trace.StateTrans) != 2 {
		t.Errorf("expected 2 state transitions")
	}

	replayer := NewTrajectoryReplayer()
	res, err := replayer.Replay(context.Background(), trace)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Errorf("expected replay to pass, got error: %s", res.Error)
	}
}

func TestTrajectoryReplayer_Failure(t *testing.T) {
	replayer := NewTrajectoryReplayer()
	trace := &TrajectoryTrace{
		SessionID: "session1",
		StateTrans: []StateTransRecord{
			{From: "A", To: "B"},
			{From: "C", To: "D"},
		},
	}
	res, err := replayer.Replay(context.Background(), trace)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Errorf("expected replay to fail")
	}
}
