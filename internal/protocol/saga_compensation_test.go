package protocol

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestSagaRecorder_OutcomeDrivesRollbackBranch 记录器的核心职责：让 FSM 的
// S_ROLLBACK 能在 OK / PARTIAL 之间做出**反映真实补偿结果**的选择。
// 这是 ADR-0088 决策一的收益点——此前 rollbackSaga 遍历恒空的 SagaLog，
// 无论补偿实际成败都返回 S_ROLLBACK_OK。
func TestSagaRecorder_OutcomeDrivesRollbackBranch(t *testing.T) {
	r := NewSagaCompensationRecorder()

	// 全部成功 → executed>0, failed==0（FSM 应走 S_ROLLBACK_OK）
	r.Record("delete_file", nil)
	r.Record("revoke_grant", nil)
	executed, failed := r.Outcome()
	if executed != 2 || failed != 0 {
		t.Fatalf("want executed=2 failed=0, got %d/%d", executed, failed)
	}
	if r.FirstError() != nil {
		t.Fatalf("all-success run must expose no error, got %v", r.FirstError())
	}

	// 掺入一条失败 → FSM 应走 S_ROLLBACK_PARTIAL 并能拿到具体原因
	sentinel := errors.New("undo target already gone")
	r.Record("refund", sentinel)
	executed, failed = r.Outcome()
	if executed != 3 || failed != 1 {
		t.Fatalf("want executed=3 failed=1, got %d/%d", executed, failed)
	}
	if !errors.Is(r.FirstError(), sentinel) {
		t.Fatalf("FirstError must surface the concrete cause for ESCALATE, got %v", r.FirstError())
	}
}

// TestSagaRecorder_NilIsNoCompensation nil 记录器（未进入过 DAG 执行、
// 或本轮无任何补偿动作）必须等价于"无补偿失败"，让 S_ROLLBACK 走 OK 分支，
// 而不是让调用方 panic 或误判为失败。
func TestSagaRecorder_NilIsNoCompensation(t *testing.T) {
	var r *SagaCompensationRecorder
	r.Record("delete_file", errors.New("boom")) // 不得 panic

	executed, failed := r.Outcome()
	if executed != 0 || failed != 0 {
		t.Fatalf("nil recorder must report zero outcome, got %d/%d", executed, failed)
	}
	if r.FirstError() != nil {
		t.Fatalf("nil recorder must expose no error, got %v", r.FirstError())
	}
}

// TestSagaRecorder_ConcurrentRecord 补偿在 runCompensation 的循环里串行写，
// 但记录器同时被 FSM 侧读取，且未来可能并行补偿——并发下不得数据竞争。
// 用 -race 运行本测试覆盖。
func TestSagaRecorder_ConcurrentRecord(t *testing.T) {
	r := NewSagaCompensationRecorder()
	const writers = 32

	var wg sync.WaitGroup
	wg.Add(writers * 2)
	for i := range writers {
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				r.Record("tool", nil)
			} else {
				r.Record("tool", errors.New("fail"))
			}
		}()
		go func() {
			defer wg.Done()
			_, _ = r.Outcome()
			_ = r.FirstError()
		}()
	}
	wg.Wait()

	executed, failed := r.Outcome()
	if executed != writers {
		t.Fatalf("want %d records, got %d", writers, executed)
	}
	if failed != writers/2 {
		t.Fatalf("want %d failures, got %d", writers/2, failed)
	}
}

// TestSagaRecorderFromContext ctx 往返：未注入返回 nil，注入后取回同一实例。
// context 是 DAG 层与 FSM 层之间唯一的传递通道（DAGRunner 接口签名不可扩展，
// 见 internal/agent/provider.go 该接口注释）。
func TestSagaRecorderFromContext(t *testing.T) {
	if got := SagaRecorderFromContext(context.Background()); got != nil {
		t.Fatal("bare context must yield a nil recorder")
	}

	r := NewSagaCompensationRecorder()
	ctx := context.WithValue(context.Background(), CtxSagaRecorderKey{}, r)
	if got := SagaRecorderFromContext(ctx); got != r {
		t.Fatal("recorder must round-trip through context unchanged")
	}
}
