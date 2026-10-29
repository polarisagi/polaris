package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

func (a *Agent) watchSubagentsCompletion(childIDs []string) { //nolint:gocyclo
	if a.handoffPoster == nil || len(childIDs) == 0 {
		return
	}
	watchCtx, cancel := context.WithCancel(a.ctx)
	a.registerHandoffWatcher(cancel)

	concurrent.SafeGo(watchCtx, "agent.subagents_watcher", func(ctx context.Context) {
		defer a.cancelHandoffWatcher()

		// 每个子 channel 由各自转发协程独占读取（单读者），终态时 watcher 通知其
		// 排空后退出并等待全部结束，之后才唤醒 FSM，保证事件不丢且顺序不乱。
		done := make(chan struct{})
		var wg sync.WaitGroup
		cancels := make([]func(), 0, len(childIDs))
		defer func() {
			for _, c := range cancels {
				c()
			}
		}()

		for _, cid := range childIDs {
			// 优先接管 SpawnAgents 在 PostTask 之前建立的预订阅。
			cEvents, cancelSub := a.subscribeChildEvents(cid)
			cancels = append(cancels, cancelSub)
			wg.Add(1)
			concurrent.SafeGo(ctx, "agent.subagents_event_forwarder", func(subCtx context.Context) {
				defer wg.Done()
				for {
					select {
					case <-subCtx.Done():
						return
					case <-done:
						a.drainChildEvents(cEvents)
						return
					case ev, ok := <-cEvents:
						if !ok {
							return
						}
						a.publishStreamEvent(ev)
					}
				}
			})
		}

		// finish 通知转发协程排空退出，全部结束后才唤醒 FSM。
		finish := func() {
			close(done)
			wg.Wait()
			a.asyncIntent(types.TriggerAgentHandoffDone)
		}

		events, err := a.handoffPoster.Subscribe(ctx)
		if err != nil {
			a.pollSubagentsCompletion(ctx, childIDs, finish)
			return
		}

		if a.peekAllSubagentsDone(ctx, childIDs) {
			finish()
			return
		}

		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-events:
				if a.peekAllSubagentsDone(ctx, childIDs) {
					finish()
					return
				}
			case <-ticker.C:
				if a.peekAllSubagentsDone(ctx, childIDs) {
					finish()
					return
				}
			}
		}
	})
}

// pollSubagentsCompletion Subscribe 不可用时的轮询降级；全部终态后走同一收尾逻辑 finish。
func (a *Agent) pollSubagentsCompletion(ctx context.Context, childIDs []string, finish func()) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.peekAllSubagentsDone(ctx, childIDs) {
				finish()
				return
			}
		}
	}
}

func (a *Agent) peekAllSubagentsDone(ctx context.Context, childIDs []string) bool {
	for _, id := range childIDs {
		snap, err := a.handoffPoster.PeekTask(ctx, id)
		if err != nil || snap == nil {
			return false
		}
		if snap.Status != types.TaskDone && snap.Status != types.TaskFailed {
			return false
		}
	}
	return true
}

func (a *Agent) truncateAndOffloadResult(ctx context.Context, taskID, resStr string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 4000
	}
	if len(resStr) <= maxChars {
		return resStr
	}

	header := fmt.Sprintf("[output truncated: %d chars total; full output not retained]\n", len(resStr))
	if a.toolOffloader != nil {
		partKey := a.memoryPartitionKey()
		if offID, err := a.toolOffloader.Offload(ctx, partKey, []byte(resStr)); err == nil {
			ref := fmt.Sprintf("read_tool_ref(task_id=%q, id=%q)", partKey, offID)
			header = fmt.Sprintf("[output truncated: %d chars total; full output: %s]\n", len(resStr), ref)
		} else {
			slog.WarnContext(ctx, "agent: subagent result offload failed", "task_id", taskID, "err", err)
		}
	}
	return header + util.ElideMiddle(resStr, maxChars-len(header))
}
