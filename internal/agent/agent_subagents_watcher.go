package agent

import (
	"context"
	"fmt"
	"log/slog"
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

		for _, cid := range childIDs {
			cID := cid
			cEvents, cancelSub := a.handoffPoster.SubscribeTaskEvents(cID)
			defer cancelSub()
			concurrent.SafeGo(ctx, "agent.subagents_event_forwarder", func(subCtx context.Context) {
				for {
					select {
					case <-subCtx.Done():
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

		events, err := a.handoffPoster.Subscribe(ctx)
		if err != nil {
			a.pollSubagentsCompletion(ctx, childIDs)
			return
		}

		if a.peekAllSubagentsDone(ctx, childIDs) {
			a.asyncIntent(types.TriggerAgentHandoffDone)
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
					a.asyncIntent(types.TriggerAgentHandoffDone)
					return
				}
			case <-ticker.C:
				if a.peekAllSubagentsDone(ctx, childIDs) {
					a.asyncIntent(types.TriggerAgentHandoffDone)
					return
				}
			}
		}
	})
}

func (a *Agent) pollSubagentsCompletion(ctx context.Context, childIDs []string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.peekAllSubagentsDone(ctx, childIDs) {
				a.asyncIntent(types.TriggerAgentHandoffDone)
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
