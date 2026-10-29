package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/repo"
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

// fakeTrajReader 内存轨迹账本：按追加顺序分配 seq，模拟 session_trajectory 的读取契约。
type fakeTrajReader struct {
	rows []repo.TrajectoryRow
	err  error
}

func (f *fakeTrajReader) add(evType, payload string) {
	f.rows = append(f.rows, repo.TrajectoryRow{
		Seq: int64(len(f.rows) + 1), SessionID: "session1", EventType: evType, Payload: payload,
	})
}

func (f *fakeTrajReader) ListBySession(_ context.Context, _ string) ([]repo.TrajectoryRow, error) {
	return f.rows, f.err
}

func TestTrajectoryRecorder(t *testing.T) {
	fr := &fakeTrajReader{}
	v1, _ := json.Marshal(map[string]any{"request": map[string]any{"a": "b"}, "response": map[string]any{"c": "d"}})
	v2, _ := json.Marshal(map[string]any{"tool": "test", "args": map[string]any{}, "result": map[string]any{}})
	fr.add("llm_call", string(v1))
	fr.add("tool_call", string(v2))
	fr.add("state_1", "{}")
	fr.add("state_2", "{}")
	recorder := NewTrajectoryRecorder(fr)

	trace, err := recorder.Record(context.Background(), "session1")
	if err != nil {
		t.Fatal(err)
	}

	if trace.SessionID != "session1" {
		t.Errorf("expected session1")
	}
	if len(trace.LLMCalls) != 1 || trace.LLMCalls[0].Request["a"] != "b" || trace.LLMCalls[0].Response["c"] != "d" {
		t.Errorf("expected 1 LLM call with request/response, got %+v", trace.LLMCalls)
	}
	if len(trace.ToolCalls) != 1 || trace.ToolCalls[0].Name != "test" {
		t.Errorf("expected 1 Tool call named test, got %+v", trace.ToolCalls)
	}
	if len(trace.StateTrans) != 2 || trace.StateTrans[1].From != "state_1" {
		t.Errorf("expected 2 chained state transitions, got %+v", trace.StateTrans)
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

// 每类 500 条上限语义保留：超出部分被丢弃且不影响其余类别；损坏的 payload 被跳过。
func TestTrajectoryRecorder_CapPerType(t *testing.T) {
	fr := &fakeTrajReader{}
	for range trajectoryTypeCap + 20 {
		fr.add("llm_call", `{"request":{},"response":{}}`)
		fr.add("tool_call", `{"tool":"t"}`)
		fr.add("3", "{}")
	}
	fr.add("tool_call", "{not json") // 损坏 payload：跳过不报错
	trace, err := NewTrajectoryRecorder(fr).Record(context.Background(), "session1")
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.LLMCalls) != trajectoryTypeCap || len(trace.ToolCalls) != trajectoryTypeCap || len(trace.StateTrans) != trajectoryTypeCap {
		t.Fatalf("每类应封顶 %d，got llm=%d tool=%d state=%d",
			trajectoryTypeCap, len(trace.LLMCalls), len(trace.ToolCalls), len(trace.StateTrans))
	}
}

func TestTrajectoryRecorder_NilReaderAndReadError(t *testing.T) {
	trace, err := NewTrajectoryRecorder(nil).Record(context.Background(), "s")
	if err != nil || trace == nil || trace.SessionID != "s" {
		t.Fatalf("nil reader 应返回空 trace: %+v %v", trace, err)
	}
	if _, err := NewTrajectoryRecorder(&fakeTrajReader{err: context.Canceled}).Record(context.Background(), "s"); err == nil {
		t.Fatal("读取失败应返回错误")
	}
}

func TestRunReplay_SeqContinuity(t *testing.T) {
	ctx := context.Background()
	fr := &fakeTrajReader{}
	fr.add("llm_call", "{}")
	fr.add("1", "{}")
	fr.add("tool_call", "{}")
	r := &RunnerImpl{}
	r.InjectTrajectoryReader(fr)
	r.replayer = NewTrajectoryReplayer()

	rep, err := r.RunReplay(ctx, "session1")
	if err != nil || !rep.Consistent || rep.DivergentOffset != -1 || rep.NewLLMCalls != 1 {
		t.Fatalf("连续轨迹应一致: %+v %v", rep, err)
	}

	// 构造缺口：seq 2 丢失。
	fr.rows = append(fr.rows[:1], fr.rows[2:]...)
	rep, err = r.RunReplay(ctx, "session1")
	if err != nil || rep.Consistent || rep.DivergentOffset != 3 {
		t.Fatalf("缺口应被检出并报首个缺口后的 seq=3: %+v %v", rep, err)
	}

	// 起点不是 1 同样是缺口（seq 1 丢失）。
	fr.rows = []repo.TrajectoryRow{{Seq: 2, EventType: "x", Payload: "{}"}}
	if rep, _ = r.RunReplay(ctx, "session1"); rep.Consistent || rep.DivergentOffset != 2 {
		t.Fatalf("首条 seq!=1 应判缺口: %+v", rep)
	}

	// 未注入读取端 / 读取失败 → 错误。
	if _, err := (&RunnerImpl{}).RunReplay(ctx, "s"); err == nil {
		t.Fatal("未注入 reader 应报错")
	}
	fr.err = context.Canceled
	if _, err := r.RunReplay(ctx, "s"); err == nil {
		t.Fatal("读取失败应报错")
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
