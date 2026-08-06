package protocol

import (
	"context"
	"sync"
	"testing"
)

// TestSagaLedger_SameUndoClaimedOnce 核心不变量：同一个 (工具, 参数) 的补偿
// 动作只能被认领一次。这是"DAG 层与 FSM 层两套并行补偿对同一 undo 各跑一遍"
// 这个数据损坏缺陷的守护点。
func TestSagaLedger_SameUndoClaimedOnce(t *testing.T) {
	l := NewSagaCompensationLedger()
	args := []byte(`{"path":"/tmp/x"}`)

	if !l.TryClaim("delete_file", args) {
		t.Fatal("first claim must succeed")
	}
	// 模拟另一条补偿路径拿着同样的 undo 动作过来
	if l.TryClaim("delete_file", args) {
		t.Fatal("second claim of the same undo must be rejected (double-compensation)")
	}
	if l.Count() != 1 {
		t.Fatalf("want 1 claimed action, got %d", l.Count())
	}
}

// TestSagaLedger_DifferentUndosBothRun 参数或工具不同即语义不同的补偿动作，
// 两者都必须执行——去重键的粒度不能粗到误伤真实需要的补偿。
func TestSagaLedger_DifferentUndosBothRun(t *testing.T) {
	l := NewSagaCompensationLedger()

	if !l.TryClaim("delete_file", []byte(`{"path":"/tmp/a"}`)) {
		t.Fatal("claim a must succeed")
	}
	if !l.TryClaim("delete_file", []byte(`{"path":"/tmp/b"}`)) {
		t.Fatal("different args must be treated as a distinct compensation")
	}
	if !l.TryClaim("refund", []byte(`{"path":"/tmp/a"}`)) {
		t.Fatal("different tool must be treated as a distinct compensation")
	}
	if l.Count() != 3 {
		t.Fatalf("want 3 distinct actions, got %d", l.Count())
	}
}

// TestSagaLedger_NilIsFailOpen 未注入账本时必须退化为"不去重"，
// 即回到引入本机制前的行为——绝不能变成"全部跳过"（那会漏补偿，
// 留下未回滚的副作用，比重复补偿更糟）。
func TestSagaLedger_NilIsFailOpen(t *testing.T) {
	var l *SagaCompensationLedger
	if !l.TryClaim("delete_file", []byte(`{}`)) {
		t.Fatal("nil ledger must fail-open (always allow), never skip compensation")
	}
	if !l.TryClaim("delete_file", []byte(`{}`)) {
		t.Fatal("nil ledger must stay fail-open across calls")
	}
	if l.Count() != 0 {
		t.Fatalf("nil ledger Count must be 0, got %d", l.Count())
	}
}

// TestSagaLedger_ConcurrentClaimExactlyOnce 两条补偿路径可能并发触达
// （DAG 的 runCompensation 在自己的 goroutine 里跑，FSM Effect 在主循环里跑），
// 并发下也必须恰好一次。用 -race 跑本测试同时覆盖数据竞争。
func TestSagaLedger_ConcurrentClaimExactlyOnce(t *testing.T) {
	l := NewSagaCompensationLedger()
	const racers = 64

	var wg sync.WaitGroup
	var granted int64
	var mu sync.Mutex

	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			if l.TryClaim("delete_file", []byte(`{"path":"/tmp/x"}`)) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != 1 {
		t.Fatalf("exactly one racer may claim the compensation, got %d", granted)
	}
}

// TestSagaLedgerFromContext ctx 往返：未注入返回 nil（fail-open），注入后取回同一实例。
func TestSagaLedgerFromContext(t *testing.T) {
	if got := SagaLedgerFromContext(context.Background()); got != nil {
		t.Fatal("bare context must yield a nil ledger (fail-open)")
	}

	l := NewSagaCompensationLedger()
	ctx := context.WithValue(context.Background(), CtxSagaLedgerKey{}, l)
	if got := SagaLedgerFromContext(ctx); got != l {
		t.Fatal("ledger must round-trip through context unchanged")
	}
}
