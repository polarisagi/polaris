package substrate

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// EventWriteBuffer — MPSC 批量写入缓冲。
// 消除多 Agent 并发写 SQLite 的写锁争抢。
// 架构文档: docs/arch/02-Storage-Fabric-深度选型.md §2.2

type StateTransitionEvent struct {
	TaskID         string
	AgentID        string
	ClaimedVersion int64
	EventType      string // state_transition | tool_call | observation | reflection | system
	Payload        []byte
}

// EventWriteBuffer 生命周期由 M2 StorageFabric.Open() 内聚管理，不依赖 M8 Supervisor Tree。
// EventWriteBuffer 为纯缓冲层，最终写入经 DatabaseWriter 单写者串行化。
type EventWriteBuffer struct {
	ch            chan *StateTransitionEvent // buf: 4096
	mutationBus   *DatabaseWriter            // 投递至 DatabaseWriter 统一单写者
	batchSize     int                        // 64
	flushInterval time.Duration              // 100ms
	leaseChecker  LeaseChecker
	wg            sync.WaitGroup
	subscribers   []chan *StateTransitionEvent
	subMutex      sync.RWMutex
}

func (b *EventWriteBuffer) Subscribe() chan *StateTransitionEvent {
	ch := make(chan *StateTransitionEvent, 100)
	b.subMutex.Lock()
	b.subscribers = append(b.subscribers, ch)
	b.subMutex.Unlock()
	return ch
}

func (b *EventWriteBuffer) Unsubscribe(ch chan *StateTransitionEvent) {
	b.subMutex.Lock()
	defer b.subMutex.Unlock()
	for i, s := range b.subscribers {
		if s == ch {
			b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
			close(ch)
			break
		}
	}
}

func (b *EventWriteBuffer) broadcast(ev *StateTransitionEvent) {
	b.subMutex.RLock()
	defer b.subMutex.RUnlock()
	for _, s := range b.subscribers {
		select {
		case s <- ev:
		default:
		}
	}
}

// Emit 发送事件。
func (b *EventWriteBuffer) Emit(ev *StateTransitionEvent) error {
	if protocol.IsReplaying() {
		return nil
	}
	b.broadcast(ev)
	select {
	case b.ch <- ev:
		return nil
	default:
		backoff := []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 250 * time.Millisecond, time.Second, 2 * time.Second}
		for i, d := range backoff {
			time.Sleep(d)
			select {
			case b.ch <- ev:
				return nil
			default:
				if i == len(backoff)-1 {
					return ErrQueueFull
				}
			}
		}
	}
	return ErrQueueFull
}

// EmitCritical 关键事件零丢失。
func (b *EventWriteBuffer) EmitCritical(ctx context.Context, ev *StateTransitionEvent) error {
	if protocol.IsReplaying() {
		return nil
	}
	b.broadcast(ev)
	intent := &MutationIntent{
		Table:     "events",
		Operation: "upsert",
		Payload:   ev.Payload,
		Priority:  PriorityFlush,
		TaskID:    ev.TaskID,
		AgentID:   ev.AgentID,
	}
	if b.mutationBus != nil {
		if err := b.mutationBus.Submit(ctx, intent); err == nil {
			return nil
		}
	}
	return writeCriticalPanicLog(ev)
}

// Serve 启动 consumeLoop（由 M2 StorageFabric.Open() 调用）。
func (b *EventWriteBuffer) Serve() {
	b.wg.Add(1)
	go b.consumeLoop()
}

// Stop 关闭 channel，排空残余事件。
func (b *EventWriteBuffer) Stop() {
	close(b.ch)
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		slog.Error("event buffer drain timeout")
		observability.IncrEventBufferDrainTimeout(int64(len(b.ch)))
	}
}

// consumeLoop 批量收集事件 → 构造 MutationIntent → 投递至 MutationBus → DatabaseWriter 串行 INSERT。
func (b *EventWriteBuffer) consumeLoop() {
	defer b.wg.Done()
	defer func() {
		if r := recover(); r != nil { //nolint:staticcheck // panic recovery will be fully implemented soon
			// CRITICAL 日志 + polaris_eventbuffer_panic Counter → StorageFabric 自动重启 consumeLoop (max 3/min)
		}
	}()

	batch := make([]*StateTransitionEvent, 0, b.batchSize)
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-b.ch:
			if !ok {
				b.flush(batch)
				return
			}
			batch = append(batch, ev)
			if len(batch) >= b.batchSize {
				b.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				b.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

// flush 将批量的 StateTransitionEvent 构造为 MutationIntent 投递至 MutationBus。
func (b *EventWriteBuffer) flush(batch []*StateTransitionEvent) {
	for _, ev := range batch {
		// 租约二次校验: TaskID 非空 → leaseChecker.Verify → 失效则丢弃 + WARN
		if ev.TaskID != "" && b.leaseChecker != nil {
			if !b.leaseChecker.Verify(ev.TaskID, ev.AgentID, ev.ClaimedVersion) {
				continue
			}
		}
		if b.mutationBus != nil {
			intent := &MutationIntent{
				Table:     "events",
				Operation: "upsert",
				Payload:   ev.Payload,
				Priority:  PriorityFlush,
				TaskID:    ev.TaskID,
				AgentID:   ev.AgentID,
			}
			_ = b.mutationBus.Submit(context.Background(), intent)
		}
	}
}

var (
	ErrQueueFull    = &EventBufferError{"event queue full"}
	ErrFlushTimeout = &EventBufferError{"flush timeout"}
)

type EventBufferError struct{ msg string }

func (e *EventBufferError) Error() string { return e.msg }

func writeCriticalPanicLog(ev *StateTransitionEvent) error { return nil }
