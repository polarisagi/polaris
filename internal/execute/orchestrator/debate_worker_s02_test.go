package orchestrator

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// failTaskAlwaysErrorsBlackboard 包一层 mockBlackboard，让 FailTask 总是失败，
// 用于验证阶段02修复：DebateWorker 在 FailTask 也失败时必须可观测（Error+counter），
// 而不是像修复前那样静默吞没。
type failTaskAlwaysErrorsBlackboard struct {
	*mockBlackboard
}

func (b *failTaskAlwaysErrorsBlackboard) FailTask(ctx context.Context, taskID, agentID string, errBytes []byte) error {
	return apperr.New(apperr.CodeInternal, "simulated FailTask failure")
}

// TestDebateWorker_InvalidIntent_FailTaskAlsoFails_S02 验证阶段02修复：当 debate
// 任务 intent JSON 非法且 FailTask 本身也失败时，GlobalBlackboardFailTaskErrorsTotal
// 必须递增（Error 级可观测），不能像修复前 `_ = w.bb.FailTask(...)` 那样彻底静默——
// 该任务此后只能靠 Reaper 的 expires_at 超时兜底，运维必须能看到这个信号。
func TestDebateWorker_InvalidIntent_FailTaskAlsoFails_S02(t *testing.T) {
	inner := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 4),
	}
	taskID := "debate-parent-1-A"
	inner.tasks[taskID] = &types.TaskEntry{
		ID:     taskID,
		Type:   DebateTaskType,
		Status: types.TaskPending,
		Intent: []byte("{not valid json"),
	}
	bb := &failTaskAlwaysErrorsBlackboard{mockBlackboard: inner}

	w := &DebateWorker{bb: bb}

	before := metrics.GlobalBlackboardFailTaskErrorsTotal.Load()
	w.tryClaimAndResume(context.Background(), taskID)
	after := metrics.GlobalBlackboardFailTaskErrorsTotal.Load()

	if after != before+1 {
		t.Errorf("expected GlobalBlackboardFailTaskErrorsTotal += 1, got before=%d after=%d", before, after)
	}
}
