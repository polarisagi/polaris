package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/automation/notify"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// storedTask 持久化到 KV 的任务快照，附带调度状态与重试计数。
// Submit 写入 "pending"；Start goroutine 消费时更新为 "running"/"completed"/"failed"。
type storedTask struct {
	Task             types.Task `json:"task"`
	Status           string     `json:"status"`                      // pending | running | completed | failed
	Attempts         int        `json:"attempts"`                    // 已尝试次数
	MissedExecutions int        `json:"missed_executions,omitempty"` // 认知负载压制期间累积的错过次数
}

type backgroundGate interface {
	BackgroundPermit(priority int) bool
}

// DispatchFn 任务投递回调，由外部 Worker/Orchestrator 注入（M13 §2.1 At-Least-Once）。
type DispatchFn func(ctx context.Context, task *types.Task) error

// SQLiteScheduler 实现了 protocol.Scheduler，作为系统的任务调度器核心。
// 架构文档: docs/arch/M13-Interface-Scheduler.md §2.1
//
// At-Least-Once 投递：
//   - Submit 将任务以 status="pending" 持久化到 KV
//   - Start 启动后台扫描 goroutine，每 scanInterval 扫一次 "scheduler:task:" 前缀
//   - 扫到 pending 任务 → CAS 更新为 "running" → 调用 dispatchFn → 成功="completed"，失败="pending"（直到 MaxAttempts）
type SQLiteScheduler struct {
	store protocol.Store

	// eventBus 用于分发任务事件给订阅者
	mu          sync.RWMutex
	subscribers map[string]map[chan types.TaskEvent]struct{}
	invoker     protocol.AgentInvoker
	gate        backgroundGate
	outbox      protocol.OutboxWriter // 可选；nil 时跳过通知投递（GD-13-001）
}

func (s *SQLiteScheduler) WithBackgroundGate(g backgroundGate) {
	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()
}

// WithOutboxWriter 注入 Outbox 写入器，用于任务终态时投递通知事件
// （GD-13-001，见 internal/automation/notify 包）。nil 时跳过通知（默认行为不变）。
func (s *SQLiteScheduler) WithOutboxWriter(w protocol.OutboxWriter) {
	s.mu.Lock()
	s.outbox = w
	s.mu.Unlock()
}

// notifyTaskTerminal 在后台/自动化任务（Pool != "intent_handler"）到达终态时
// 写入一条 Outbox 通知事件，由 internal/automation/notify.Dispatcher 消费投递。
//
// 判定依据（GD-13-001 最小实现范围）：用户交互式任务（Pool=="intent_handler"，
// types.Task.Pool 文档："0=最高(用户交互)"）已经通过 SSE 实时可见，不需要重复
// 通知；只对真正的后台/自动化/长程任务（background/cron/eval/ingest）发通知，
// 不做更复杂的"用户是否在线"判断（该判断留给用户在通知偏好里选择"从不通知"）。
//
// 单条通知写入失败只记录日志，不影响任务本身的终态持久化（通知是旁路能力，
// 不应该让核心调度语义依赖它）。
func (s *SQLiteScheduler) notifyTaskTerminal(ctx context.Context, task types.Task, success bool, errMsg string) {
	s.mu.RLock()
	ob := s.outbox
	s.mu.RUnlock()
	if ob == nil || task.Pool == "intent_handler" {
		return
	}

	ev := notify.NotificationEvent{
		TaskID:    task.ID,
		TaskType:  task.Type,
		Pool:      task.Pool,
		Success:   success,
		Error:     errMsg,
		Timestamp: time.Now().UnixMilli(),
	}
	entry, err := protocol.NewOutboxEvent(protocol.TopicNotification, "notify", ev, "notify:"+task.ID)
	if err != nil {
		slog.Error("scheduler: build notification outbox event failed", "task_id", task.ID, "err", err)
		return
	}
	if err := ob.Write(ctx, entry); err != nil {
		slog.Error("scheduler: write notification outbox event failed", "task_id", task.ID, "err", err)
	}
}

var _ protocol.Scheduler = (*SQLiteScheduler)(nil)

func (s *SQLiteScheduler) SetAgentInvoker(invoker protocol.AgentInvoker) {
	s.mu.Lock()
	s.invoker = invoker
	s.mu.Unlock()
}

func NewSQLiteScheduler(store protocol.Store) *SQLiteScheduler {
	return &SQLiteScheduler{
		store:       store,
		subscribers: make(map[string]map[chan types.TaskEvent]struct{}),
	}
}

// Start 启动后台消费者 goroutine，实现 At-Least-Once 投递（M13 §2.1）。
// dispatchFn 由 M8 Orchestrator 注入，负责实际任务执行。
// 进程退出时 ctx 取消，goroutine 自动退出；重启后扫到 status="pending"/"running" 的任务自动重试。
func (s *SQLiteScheduler) Start(ctx context.Context, dispatchFn DispatchFn) {
	const scanInterval = 5 * time.Second
	// [SafeGo] At-Least-Once 投递的心脏：单个 tick 内 panic（如畸形 storedTask 反序列化
	// 边界情况）此前会直接打穿整个进程，现改为 recover 后记录日志，不再影响其余子系统存活。
	concurrent.SafeGo(ctx, "automation.scheduler.scan_loop", func(ctx context.Context) {
		ticker := time.NewTicker(scanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.scanAndDispatch(ctx, dispatchFn)
			}
		}
	})
}

// scanAndDispatch 扫描一轮 pending/running（可能是崩溃后残留）任务并投递。
//
//nolint:gocyclo
func (s *SQLiteScheduler) scanAndDispatch(ctx context.Context, dispatchFn DispatchFn) {
	prefix := []byte("scheduler:task:")
	iter, err := s.store.Scan(ctx, prefix)
	if err != nil {
		slog.Warn("scheduler: scan pending tasks failed", "err", err)
		return
	}
	var stList []storedTask
	for iter.Next() {
		val := iter.Value()
		var st storedTask
		if err := json.Unmarshal(val, &st); err != nil {
			continue
		}
		stList = append(stList, st)
	}
	iter.Close() // 显式关闭迭代器，释放读事务锁，避免下面写更新时死锁

	for _, st := range stList {

		maxAttempts := st.Task.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 3
		}
		// 跳过已完成 / 已超出重试次数
		if st.Status == "completed" || st.Status == "failed" {
			continue
		}
		if st.Attempts >= maxAttempts {
			// 超出重试次数，标记 failed 并通知
			st.Status = "failed"
			if err := s.writeTask(ctx, &st); err != nil {
				slog.Error("scheduler: persist failed task state error", "task_id", st.Task.ID, "err", err)
			}
			s.publish(st.Task.ID, types.TaskEvent{TaskID: st.Task.ID, State: "failed"})
			s.notifyTaskTerminal(ctx, st.Task, false, "max attempts exceeded")
			continue
		}

		// CC-2 内稳态防抖：高认知负载时累积 miss，焦点解除后仅补偿执行一次
		taskPriority := st.Task.Priority
		if taskPriority <= 0 {
			taskPriority = 2 // 默认后台优先级
		}
		if s.gate != nil && taskPriority >= 2 && !s.gate.BackgroundPermit(taskPriority) {
			// 负载过高：原地累积 missed_executions，不推进到 running
			st.MissedExecutions++
			if err := s.writeTask(ctx, &st); err != nil {
				slog.Error("scheduler: persist deferred task state error", "task_id", st.Task.ID, "err", err)
			}
			slog.Debug("scheduler: task deferred due to cognitive load",
				"task_id", st.Task.ID,
				"priority", taskPriority,
				"missed", st.MissedExecutions,
			)
			continue
		}
		// 焦点解除：若曾有积压，记录补偿执行日志并清零（只触发一次，不补跑 N 次）
		if st.MissedExecutions > 0 {
			slog.Info("scheduler: compensating deferred task",
				"task_id", st.Task.ID,
				"missed_executions", st.MissedExecutions,
			)
			st.MissedExecutions = 0
		}

		// CAS：更新为 running（防止多节点重复调度）
		st.Status = "running"
		st.Attempts++
		if err := s.writeTask(ctx, &st); err != nil {
			continue
		}
		s.publish(st.Task.ID, types.TaskEvent{TaskID: st.Task.ID, State: "started"})

		taskCopy := st.Task
		concurrent.SafeGo(ctx, "automation.scheduler.dispatch_task", func(ctx context.Context) {
			var dispErr error
			s.mu.RLock()
			inv := s.invoker
			s.mu.RUnlock()
			if inv != nil && taskCopy.Type == "agent" {
				_, dispErr = inv.InvokeAgent(ctx, string(taskCopy.Payload))
			} else {
				dispErr = dispatchFn(ctx, &taskCopy)
			}

			if dispErr == nil {
				st.Status = "completed"
				if err := s.writeTask(ctx, &st); err != nil {
					slog.Error("scheduler: persist completed task state error", "task_id", st.Task.ID, "err", err)
				}
				s.publish(st.Task.ID, types.TaskEvent{TaskID: st.Task.ID, State: "completed"})
				s.notifyTaskTerminal(ctx, taskCopy, true, "")
			} else {
				// 失败回写为 pending，下轮扫描重试
				slog.Warn("scheduler: dispatch failed, will retry", "task_id", st.Task.ID, "attempts", st.Attempts, "err", dispErr)
				st.Status = "pending"
				if err := s.writeTask(ctx, &st); err != nil {
					slog.Error("scheduler: persist retry task state error", "task_id", st.Task.ID, "err", err)
				}
			}
		})
	}
}

func (s *SQLiteScheduler) writeTask(ctx context.Context, st *storedTask) error {
	data, err := json.Marshal(st)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteScheduler.writeTask", err)
	}
	return s.store.Put(ctx, []byte("scheduler:task:"+st.Task.ID), data)
}

func (s *SQLiteScheduler) Submit(ctx context.Context, task types.Task) (string, error) {
	if task.ID == "" {
		task.ID = fmt.Sprintf("task_%d", time.Now().UnixNano())
	}
	st := storedTask{Task: task, Status: "pending", Attempts: 0}
	if err := s.writeTask(ctx, &st); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteScheduler.Submit", err)
	}

	s.publish(task.ID, types.TaskEvent{TaskID: task.ID, State: "submitted"})
	return task.ID, nil
}

func (s *SQLiteScheduler) Get(ctx context.Context, id string) (*types.Task, error) {
	key := []byte("scheduler:task:" + id)
	data, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteScheduler.Get", err)
	}
	// 兼容旧格式（直接序列化 types.Task）和新格式（storedTask 包装）
	var st storedTask
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteScheduler.Get", err)
	}
	if st.Task.ID != "" {
		return &st.Task, nil
	}
	// 降级：旧格式直接解析为 Task
	var task types.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteScheduler.Get", err)
	}
	return &task, nil
}

func (s *SQLiteScheduler) Cancel(ctx context.Context, id string) error {
	// MVP: 删除记录并通知 cancel 事件
	key := []byte("scheduler:task:" + id)
	err := s.store.Delete(ctx, key)
	if err == nil {
		s.publish(id, types.TaskEvent{
			TaskID: id,
			State:  "cancelled",
		})
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteScheduler.Cancel", err)
	}
	return nil
}

func (s *SQLiteScheduler) Subscribe(ctx context.Context, taskID string) (<-chan types.TaskEvent, error) {
	ch := make(chan types.TaskEvent, 16)

	s.mu.Lock()
	if s.subscribers[taskID] == nil {
		s.subscribers[taskID] = make(map[chan types.TaskEvent]struct{})
	}
	s.subscribers[taskID][ch] = struct{}{}
	s.mu.Unlock()

	// 清理逻辑
	concurrent.SafeGo(ctx, "automation.scheduler.subscriber_cleanup", func(ctx context.Context) {
		<-ctx.Done()
		s.mu.Lock()
		if subs, ok := s.subscribers[taskID]; ok {
			delete(subs, ch)
			if len(subs) == 0 {
				delete(s.subscribers, taskID)
			}
		}
		s.mu.Unlock()
		close(ch)
	})

	return ch, nil
}

func (s *SQLiteScheduler) publish(taskID string, ev types.TaskEvent) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	subs, ok := s.subscribers[taskID]
	if !ok {
		return
	}
	for ch := range subs {
		select {
		case ch <- ev:
		default: // 背压丢弃
		}
	}
}
