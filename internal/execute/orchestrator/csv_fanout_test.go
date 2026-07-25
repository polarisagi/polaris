package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// autoCompleteBlackboard 是 mockBlackboard 的装饰：PostBatch 时立即把每个
// TaskEntry 标记为 TaskDone，供大规模行数的流式化回归测试使用（无需真实
// worker 认领/完成，避免测试本身成为性能瓶颈）。
type autoCompleteBlackboard struct {
	*mockBlackboard
}

func (a *autoCompleteBlackboard) PostBatch(ctx context.Context, tasks []*types.TaskEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range tasks {
		t.Status = types.TaskDone
		a.tasks[t.ID] = t
	}
	return nil
}

// TestCSVFanout_LargeFile_StreamsInBoundedBatches 验证 D6 流式重写后能正确
// 处理远大于单批（concurrency*4）的行数——旧实现用 r.ReadAll() 一次性加载
// 整个文件，本测试确保改为逐批读取后功能等价（全部行正确聚合到 Done），
// 不因批次切分丢行、重复或状态错乱。
func TestCSVFanout_LargeFile_StreamsInBoundedBatches(t *testing.T) {
	// waitForTask 以 500ms ticker 轮询任务状态（即使 mock 立即完成任务，也要等
	// 到下一次 tick 才能观察到），故并发度需远大于行数/超时预算的比值，否则
	// 测试本身会被轮询下限拖慢，而非验证流式批次逻辑。concurrency=50 →
	// batchSize=200，rowCount=300 强制跨 2 个批次，预期总耗时 ≈300/50*0.5s=3s。
	const rowCount = 300
	var sb strings.Builder
	sb.WriteString("id,name,value\n")
	for i := 0; i < rowCount; i++ {
		fmt.Fprintf(&sb, "%d,item%d,%d\n", i, i, i*10)
	}
	path := "test_fanout_large.csv"
	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	job := CSVFanoutJob{
		CSVPath:        path,
		IDColumn:       "id",
		Instruction:    "process {name}",
		MaxConcurrency: 50, // batchSize = 50*4 = 200 < rowCount，强制跨批次
	}

	b := &autoCompleteBlackboard{mockBlackboard: &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, rowCount+10),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := RunCSVFanout(ctx, b, job)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.Total != rowCount {
		t.Errorf("expected Total=%d, got %d", rowCount, res.Total)
	}
	if res.Done != rowCount {
		t.Errorf("expected Done=%d, got %d (Errors=%d)", rowCount, res.Done, res.Errors)
	}
	if len(res.Rows) != rowCount {
		t.Errorf("expected %d row results, got %d", rowCount, len(res.Rows))
	}
}

// TestCSVFanout_ContextCanceled_ReturnsPromptly 验证 GR-7-005 修复：信号量
// 获取现在受 ctx.Done() 保护，即使行数远大于并发度，ctx 提前超时时主循环也
// 必须在有限时间内返回，而不是永久阻塞在满载的 sem channel 上。
func TestCSVFanout_ContextCanceled_ReturnsPromptly(t *testing.T) {
	const rowCount = 200
	var sb strings.Builder
	sb.WriteString("id,name\n")
	for i := 0; i < rowCount; i++ {
		fmt.Fprintf(&sb, "%d,item%d\n", i, i)
	}
	path := "test_fanout_cancel.csv"
	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	job := CSVFanoutJob{
		CSVPath:        path,
		IDColumn:       "id",
		Instruction:    "process {name}",
		MaxConcurrency: 2,
		MaxRuntimeSec:  1, // 强制内部 waitCtx 在 1s 内到期，任务永不完成（mockBlackboard 不 auto-complete）
	}

	b := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, rowCount+10),
	}

	done := make(chan struct{})
	var res *FanoutResult
	var err error
	go func() {
		res, err = RunCSVFanout(context.Background(), b, job)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunCSVFanout did not return within bound after ctx deadline — possible semaphore deadlock (GR-7-005 regression)")
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	// 一旦 waitCtx 到期，实现按设计停止继续从文件读取后续行（不为了凑总数而
	// 强制扫完整个文件，这正是流式化要避免的"为报告一个数字而扫全文件"开销）。
	// 因此 Total 只反映到期前已读入并处理的行数，不等于文件总行数 rowCount；
	// 断言重点是：(a) 已处理的行全部因超时被标记为 error（无遗留 pending/
	// running 悬空状态），(b) 确有部分行被处理（证明不是一行没处理就整体
	// 失败退出）。
	if res.Total == 0 {
		t.Fatal("expected at least some rows to be read/processed before cancellation")
	}
	if res.Total > rowCount {
		t.Errorf("Total=%d must not exceed file row count %d", res.Total, rowCount)
	}
	if res.Errors != res.Total {
		t.Errorf("expected all processed rows (%d) to end in error after deadline, got Errors=%d", res.Total, res.Errors)
	}
	if len(res.Rows) != res.Total {
		t.Errorf("Rows length %d must match Total %d", len(res.Rows), res.Total)
	}
}

func TestCSVFanoutJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Write a mock CSV file
	csvData := `id,name,value
1,alice,10
2,bob,20
3,charlie,30`
	err := os.WriteFile("test_fanout.csv", []byte(csvData), 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove("test_fanout.csv")

	job := CSVFanoutJob{
		CSVPath:        "test_fanout.csv",
		OutputCSVPath:  "test_fanout_out.csv",
		IDColumn:       "id",
		Instruction:    "Hello {name}, value is {value}",
		MaxConcurrency: 2,
	}

	b := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 100),
	}
	res, err := RunCSVFanout(ctx, b, job)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	if res.Errors != 3 {
		t.Errorf("expected 3 failed rows, got %d", res.Errors)
	}
}
