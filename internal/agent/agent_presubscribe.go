package agent

import (
	"context"
	"sync"

	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// preSub 子任务事件的预订阅：先于 PostTask 建立，watcher 启动时接管。
type preSub struct {
	ch     <-chan types.AgentStreamEvent
	cancel func()
}

// preSubscribeTable 预订阅表。独立成结构体，避免 Agent 字段继续膨胀。
type preSubscribeTable struct {
	mu   sync.Mutex
	subs map[string]preSub
	once sync.Once // 首次登记时启动生命周期清理协程
}

// preSubscribeChild 在 PostTask 之前订阅子任务事件。
// 为什么：子任务投递后可能被 Worker 极速认领，事件产生早于 watcher 订阅会被
// PublishTaskEvent 丢弃；先订阅后投递即可保证从第一条事件起不丢。
func (a *Agent) preSubscribeChild(childID string) {
	if a.handoffPoster == nil || childID == "" {
		return
	}
	ch, cancel := a.handoffPoster.SubscribeTaskEvents(childID)
	t := &a.preSubs
	t.mu.Lock()
	if t.subs == nil {
		t.subs = make(map[string]preSub)
	}
	old, hadOld := t.subs[childID]
	t.subs[childID] = preSub{ch: ch, cancel: cancel}
	t.mu.Unlock()
	if hadOld && old.cancel != nil {
		old.cancel()
	}
	// Agent 结束时注销残留预订阅（wait=none 以外的路径正常由 watcher 接管；
	// 批量委派中途失败等路径依赖此兜底）。
	t.once.Do(func() {
		concurrent.SafeGo(a.ctx, "agent.presub_reaper", func(ctx context.Context) {
			<-ctx.Done()
			a.releaseAllPreSubs()
		})
	})
}

// takePreSub 取出并移除预订阅；不存在时 ok=false（调用方现场订阅）。
func (a *Agent) takePreSub(childID string) (ch <-chan types.AgentStreamEvent, cancel func(), ok bool) {
	t := &a.preSubs
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.subs[childID]
	if !ok {
		return nil, nil, false
	}
	delete(t.subs, childID)
	return s.ch, s.cancel, true
}

// cancelPreSubs 注销指定子任务的预订阅（PostTask 失败/级联取消路径）。
func (a *Agent) cancelPreSubs(childIDs ...string) {
	for _, id := range childIDs {
		if _, cancel, ok := a.takePreSub(id); ok && cancel != nil {
			cancel()
		}
	}
}

// releaseAllPreSubs 注销全部残留预订阅。
func (a *Agent) releaseAllPreSubs() {
	t := &a.preSubs
	t.mu.Lock()
	subs := t.subs
	t.subs = nil
	t.mu.Unlock()
	for _, s := range subs {
		if s.cancel != nil {
			s.cancel()
		}
	}
}

// subscribeChildEvents 取子任务事件订阅：优先接管预订阅，否则现场订阅
// （恢复路径，早期事件丢失可接受）。
func (a *Agent) subscribeChildEvents(childID string) (<-chan types.AgentStreamEvent, func()) {
	if ch, cancel, ok := a.takePreSub(childID); ok {
		return ch, cancel
	}
	return a.handoffPoster.SubscribeTaskEvents(childID)
}
