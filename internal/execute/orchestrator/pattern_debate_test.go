package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// setupDebateBlackboard 复用 setupPatternBlackboard 的 tasks/events 表，
// 额外建立 DebateExecutor 依赖的 task_checkpoints 表（DDL 与
// pattern_state_graph_checkpoint_test.go 保持一致）。
func setupDebateBlackboard(t *testing.T) *SQLiteBlackboard {
	t.Helper()
	bb := setupPatternBlackboard(t)
	return bb
}

// TestDebateExecutor_FullRoundTrip 验证 GD-6 PatternDebate 状态机在
// "每次子任务完成后被重新调用 Execute" 的前提下，能正确走完
// judge_init -> proponent -> opponent -> judge_final 全流程并产出结论。
//
// 驱动顺序是状态机内部固定的（judge_init 一次、proponent 一次、opponent
// 一次、maxRounds=1 时 judge_final 一次，共 4 次投递），测试按投递序号
// 匹配对应结果，不依赖对 Intent/Type 内容做模糊猜测。
//
// 注意：本测试手动模拟"外部重调用"（一个真实调度循环应有的行为），
// 不代表生产环境已具备该驱动能力——见 pattern_debate.go 顶部的已知缺口说明。
func TestDebateExecutor_FullRoundTrip(t *testing.T) {
	bb := setupDebateBlackboard(t)
	de := NewDebateExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const parentTaskID = "debate-parent-1"
	proponent := types.TaskEntry{Intent: []byte("正方：应该采用方案 A")}
	opponent := types.TaskEntry{Intent: []byte("反方：应该采用方案 B")}
	judge := types.TaskEntry{Intent: []byte("请综合双方论点给出裁决")}

	// 投递顺序固定：judge_init -> proponent -> opponent -> judge_final。
	resultsInOrder := [][]byte{
		[]byte("议题：方案 A vs 方案 B"),
		[]byte("方案 A 更省资源"),
		[]byte("方案 B 更安全"),
		[]byte("裁决：采纳方案 A，附加方案 B 的安全约束"),
	}
	finalVerdict := resultsInOrder[len(resultsInOrder)-1]

	postedCount := 0
	var lastVerdict []byte
	var lastErr error
	for range 8 { // 安全上限，正常应在 4 轮内收敛
		lastVerdict, lastErr = de.Execute(ctx, parentTaskID, proponent, opponent, judge, 1)
		if lastErr == nil {
			break
		}

		row := bb.DB().QueryRowContext(ctx,
			`SELECT task_id FROM tasks WHERE status='pending' ORDER BY rowid DESC LIMIT 1`)
		var taskID string
		if scanErr := row.Scan(&taskID); scanErr != nil {
			t.Fatalf("no in-flight task found to complete (execute err=%v): %v", lastErr, scanErr)
		}
		if postedCount >= len(resultsInOrder) {
			t.Fatalf("debate posted more tasks (%d) than expected (%d)", postedCount+1, len(resultsInOrder))
		}
		result := resultsInOrder[postedCount]
		postedCount++

		if _, claimErr := bb.ClaimTask(ctx, taskID, "test-worker"); claimErr != nil {
			t.Fatalf("claim task %s failed: %v", taskID, claimErr)
		}
		if compErr := bb.CompleteTask(ctx, taskID, "test-worker", result); compErr != nil {
			t.Fatalf("complete task %s failed: %v", taskID, compErr)
		}
	}

	if lastErr != nil {
		t.Fatalf("debate did not converge within safety bound, last err: %v", lastErr)
	}
	if string(lastVerdict) != string(finalVerdict) {
		t.Errorf("expected final verdict %q, got %q", finalVerdict, lastVerdict)
	}
	if postedCount != len(resultsInOrder) {
		t.Errorf("expected exactly %d sub-tasks posted, got %d", len(resultsInOrder), postedCount)
	}

	// 收敛后再次调用 Execute 应直接返回缓存的 verdict（JudgeFinalized 短路）。
	verdict2, err := de.Execute(ctx, parentTaskID, proponent, opponent, judge, 1)
	if err != nil {
		t.Fatalf("expected finalized verdict to short-circuit without error, got %v", err)
	}
	if string(verdict2) != string(finalVerdict) {
		t.Errorf("expected cached verdict on re-call, got %q", verdict2)
	}
}
