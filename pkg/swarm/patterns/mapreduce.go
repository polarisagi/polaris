package patterns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/swarm"
)

// MapReduceExecutor 分片归并执行器。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
// Map: 将父任务按 Scope 拆分后投递至黑板
// Reduce: 收集 Result，去重 Artifacts hash，聚合结果写回。
type MapReduceExecutor struct {
	bb           *swarm.SQLiteBlackboard
	totalTimeout time.Duration // 全局等待超时，默认 10min
}

// NewMapReduceExecutor 创建 MapReduceExecutor，totalTimeout=0 使用默认值 10min。
func NewMapReduceExecutor(bb *swarm.SQLiteBlackboard, totalTimeout time.Duration) *MapReduceExecutor {
	if totalTimeout <= 0 {
		totalTimeout = 10 * time.Minute
	}
	return &MapReduceExecutor{bb: bb, totalTimeout: totalTimeout}
}

// Execute 接收已经拆分好的子任务，并行执行后进行 Reduce 收集。
func (mre *MapReduceExecutor) Execute(ctx context.Context, parentTaskID string, subTasks []protocol.TaskEntry) ([]byte, error) { //nolint:gocyclo
	if len(subTasks) == 0 {
		return nil, nil
	}

	// 1. 先订阅黑板事件，再投递任务，防止投递后错过事件
	events, err := mre.bb.Subscribe(ctx)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "failed to subscribe", err)
	}

	for i := range subTasks {
		task := &subTasks[i]
		if err := mre.bb.PostTask(ctx, task); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("failed to post map task %s", task.ID), err)
		}
	}

	pendingMap := make(map[string]bool)
	for _, task := range subTasks {
		pendingMap[task.ID] = true
	}

	results := make([][]byte, 0, len(subTasks))
	seenHashes := make(map[string]bool)

	for len(pendingMap) > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil, perrors.New(perrors.CodeInternal, "events channel closed unexpectedly")
			}
			if pendingMap[ev.TaskID] {
				if ev.Type == "task_completed" {
					hash := sha256.Sum256(ev.Payload)
					hashStr := hex.EncodeToString(hash[:])
					if !seenHashes[hashStr] {
						seenHashes[hashStr] = true
						results = append(results, ev.Payload)
					}
					delete(pendingMap, ev.TaskID)
				} else if ev.Type == "task_failed" {
					return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("map task %s failed: %s", ev.TaskID, string(ev.Payload)))
				}
			}
		case <-time.After(mre.totalTimeout):
			return nil, perrors.New(perrors.CodeInternal, "mapreduce execution timeout")
		}
	}

	var aggregated []byte
	for i, res := range results {
		aggregated = append(aggregated, []byte(fmt.Sprintf("\n--- Result %d ---\n", i))...)
		aggregated = append(aggregated, res...)
	}

	return aggregated, nil
}
