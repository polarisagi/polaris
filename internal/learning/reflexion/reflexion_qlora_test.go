package reflexion

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/learning"
	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/internal/protocol"
)

func TestBuildQLoRASample_ValidTrajectory(t *testing.T) {
	traj := []learning.Step{
		{Index: 0, Action: "尝试直接查询", Result: "报错: 表不存在", Success: false},
		{Index: 1, Action: "改用正确表名重试", Reasoning: "上一步用错了表名", Result: "查询成功，返回 3 行", Success: true},
	}
	sample, ok := buildQLoRASample(traj)
	if !ok {
		t.Fatal("expected buildQLoRASample to succeed")
	}
	if sample.Completion != "查询成功，返回 3 行" {
		t.Errorf("unexpected completion: %q", sample.Completion)
	}
	if sample.Prompt == "" {
		t.Error("expected non-empty prompt built from prior context")
	}
}

func TestBuildQLoRASample_SingleStepFallsBackToAction(t *testing.T) {
	traj := []learning.Step{
		{Index: 0, Action: "一次性完成任务", Result: "成功", Success: true},
	}
	sample, ok := buildQLoRASample(traj)
	if !ok {
		t.Fatal("expected buildQLoRASample to succeed for single-step trajectory")
	}
	if sample.Prompt != "一次性完成任务" {
		t.Errorf("expected prompt to fall back to Action, got %q", sample.Prompt)
	}
	if sample.Completion != "成功" {
		t.Errorf("unexpected completion: %q", sample.Completion)
	}
}

func TestBuildQLoRASample_RejectsEmptyOrFailingTrajectory(t *testing.T) {
	if _, ok := buildQLoRASample(nil); ok {
		t.Error("expected empty trajectory to be rejected")
	}
	failing := []learning.Step{{Index: 0, Action: "a", Result: "r", Success: false}}
	if _, ok := buildQLoRASample(failing); ok {
		t.Error("expected trajectory ending in failure to be rejected")
	}
	noResult := []learning.Step{{Index: 0, Action: "a", Result: "", Success: true}}
	if _, ok := buildQLoRASample(noResult); ok {
		t.Error("expected trajectory with empty final Result to be rejected")
	}
}

// fakeQLoRAAdapter 记录 Train 调用次数与最后一批样本，避免测试真实发起 HTTP 请求。
// Train 由 TrainingSampleCollector 经 concurrent.SafeGo 异步调用，故需加锁。
type fakeQLoRAAdapter struct {
	mu      sync.Mutex
	calls   int
	samples []protocol.TrainingSample
}

func (f *fakeQLoRAAdapter) Train(ctx context.Context, samples []protocol.TrainingSample) (*protocol.TrainingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.samples = append(f.samples, samples...)
	return &protocol.TrainingResult{JobID: "job"}, nil
}

func (f *fakeQLoRAAdapter) getCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestReflect_ReplaySuccess_RecordsQLoRASample(t *testing.T) {
	fake := &fakeQLoRAAdapter{}
	collector := llmadapter.NewTrainingSampleCollector("qlora", fake, 1) // batchSize=1：立即触发
	engine := NewReflexionEngine(nil, nil, nil)
	engine.InjectQLoRACollector(collector)

	traj := []learning.Step{
		{Index: 0, Action: "初次尝试", Result: "失败", Success: false},
		{Index: 1, Action: "纠正后重试", Result: "成功完成任务", Success: true},
	}
	result := &learning.TaskResult{TaskID: "t1", Success: true}

	if _, err := engine.Reflect(context.Background(), "t1", "coding", result, traj, 1); err != nil {
		t.Fatalf("Reflect returned error: %v", err)
	}

	// collector.Add 本身在 replaySuccess 内同步调用（早于异步 insight 提炼
	// goroutine），但 Add 达到 batchSize 后触发的 Train() 调用是
	// TrainingSampleCollector 内部经 concurrent.SafeGo 异步执行的，故轮询等待。
	deadline := time.Now().Add(2 * time.Second)
	for fake.getCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fake.getCalls(); got != 1 {
		t.Fatalf("expected QLoRAAdapter.Train to be triggered once, got %d calls", got)
	}
	fake.mu.Lock()
	samples := fake.samples
	fake.mu.Unlock()
	if len(samples) != 1 || samples[0].Completion != "成功完成任务" {
		t.Errorf("unexpected training sample: %+v", samples)
	}
}

func TestReflect_NoReplan_DoesNotRecordSample(t *testing.T) {
	fake := &fakeQLoRAAdapter{}
	collector := llmadapter.NewTrainingSampleCollector("qlora", fake, 1)
	engine := NewReflexionEngine(nil, nil, nil)
	engine.InjectQLoRACollector(collector)

	traj := []learning.Step{{Index: 0, Action: "一次成功", Result: "成功", Success: true}}
	result := &learning.TaskResult{TaskID: "t2", Success: true}

	// replanCount=0：未经历"犯错→纠偏"，不应被视为 QLoRA 样本
	if _, err := engine.Reflect(context.Background(), "t2", "coding", result, traj, 0); err != nil {
		t.Fatalf("Reflect returned error: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // 给可能误触发的异步路径留出时间，确保断言稳定
	if got := fake.getCalls(); got != 0 {
		t.Errorf("expected no training trigger without replan, got %d calls", got)
	}
}
