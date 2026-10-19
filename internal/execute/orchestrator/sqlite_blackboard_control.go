package orchestrator

import (
	"sync"

	"github.com/polarisagi/polaris/pkg/types"
)

// SubscribeTaskControl 订阅指定任务的独立控制指令通道（R3-04）。
// 控制通道只承载父 Agent 发出的指令（steer / cancel），与事件流完全解耦，消除回环。
func (bb *SQLiteBlackboard) SubscribeTaskControl(taskID string) (<-chan types.TaskControl, func()) {
	ch := make(chan types.TaskControl, 16)
	bb.subMu.Lock()
	if bb.taskCtrlSubs == nil {
		bb.taskCtrlSubs = make(map[string][]chan types.TaskControl)
	}
	bb.taskCtrlSubs[taskID] = append(bb.taskCtrlSubs[taskID], ch)
	bb.subMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			bb.subMu.Lock()
			defer bb.subMu.Unlock()
			subs := bb.taskCtrlSubs[taskID]
			for i, s := range subs {
				if s == ch {
					subs = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			if len(subs) == 0 {
				delete(bb.taskCtrlSubs, taskID)
			} else {
				bb.taskCtrlSubs[taskID] = subs
			}
			close(ch)
		})
	}
	return ch, cancel
}

// SendTaskControl 向指定任务投递控制指令（插话 / 中止，R3-04）。
// 必须在持有 subMu 读锁期间完成非阻塞发送，消除与 cancel 侧 close(ch) 的竞态。
// 返回是否成功投递给至少一个活跃订阅者。
func (bb *SQLiteBlackboard) SendTaskControl(taskID string, ctrl types.TaskControl) bool {
	bb.subMu.RLock()
	defer bb.subMu.RUnlock()
	subs := bb.taskCtrlSubs[taskID]
	if len(subs) == 0 {
		return false
	}
	delivered := false
	for _, ch := range subs {
		select {
		case ch <- ctrl:
			delivered = true
		default:
		}
	}
	return delivered
}
