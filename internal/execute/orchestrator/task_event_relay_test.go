package orchestrator

import (
	"sync"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// TestPublishTaskEvent_CancelDuringPublish 守护 GD-13-001 子事件透传的核心竞态。
//
// 缺陷形态（本机制初版）：PublishTaskEvent 先在锁内复制订阅者切片、释放锁后再
// 向 channel 发送；cancel() 在锁内 close(ch)。两者交错即 panic: send on closed
// channel。这在生产中几乎必然发生——父 Agent 收到 task_completed 后 defer
// cancelSub()，而子 Agent 的收尾事件正由 pool 的 stream goroutine 同时投递。
//
// 用 -race 跑本测试同时覆盖数据竞争与 panic 两种失败形态。
func TestPublishTaskEvent_CancelDuringPublish(t *testing.T) {
	bb := &SQLiteBlackboard{}
	const rounds = 200

	for range rounds {
		_, cancel := bb.SubscribeTaskEvents("task-1")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				bb.PublishTaskEvent("task-1", types.AgentStreamEvent{Content: "tick"})
			}
		}()
		go func() {
			defer wg.Done()
			cancel()
		}()
		wg.Wait()
	}
}

// TestSubscribeTaskEvents_CancelIsIdempotent 重复 cancel 不得 double-close。
// watchHandoffCompletion 里 cancelSub 走 defer，降级路径也可能再次触发。
func TestSubscribeTaskEvents_CancelIsIdempotent(t *testing.T) {
	bb := &SQLiteBlackboard{}
	_, cancel := bb.SubscribeTaskEvents("task-2")
	cancel()
	cancel() // 不得 panic
}

// TestSubscribeTaskEvents_NoMapKeyLeak 取消后必须连 map key 一起删除。
// 每次 transfer_to_agent 委派都会新建一个 taskID key，只留空 slice 会让
// taskEventSubs 随委派次数无界增长（Tier-0 2GB 约束下是真实常驻泄漏）。
func TestSubscribeTaskEvents_NoMapKeyLeak(t *testing.T) {
	bb := &SQLiteBlackboard{}
	for i := range 100 {
		_, cancel := bb.SubscribeTaskEvents(string(rune('a'+i%26)) + string(rune('0'+i/26)))
		cancel()
	}
	bb.subMu.RLock()
	n := len(bb.taskEventSubs)
	bb.subMu.RUnlock()
	if n != 0 {
		t.Fatalf("taskEventSubs must be empty after all cancels, got %d keys", n)
	}
}

// TestPublishTaskEvent_MultipleSubscribersAndBackpressure 多订阅者广播 +
// 满 buffer 时丢弃而非阻塞（不得卡住子 Agent 的执行）。
func TestPublishTaskEvent_MultipleSubscribersAndBackpressure(t *testing.T) {
	bb := &SQLiteBlackboard{}
	ch1, cancel1 := bb.SubscribeTaskEvents("task-3")
	defer cancel1()
	ch2, cancel2 := bb.SubscribeTaskEvents("task-3")
	defer cancel2()

	// 远超 buffer(64)：多出的部分必须被丢弃，调用不得阻塞。
	for range 500 {
		bb.PublishTaskEvent("task-3", types.AgentStreamEvent{Content: "x"})
	}
	if len(ch1) != 64 || len(ch2) != 64 {
		t.Fatalf("want both subscribers filled to buffer cap 64, got %d/%d", len(ch1), len(ch2))
	}
}
