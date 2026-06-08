package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// SQLiteScheduler 实现了 protocol.Scheduler，作为系统的任务调度器核心。
// 架构文档: docs/arch/M13-Interface-Scheduler.md §2.1
type SQLiteScheduler struct {
	store protocol.Store

	// eventBus 用于分发任务事件给订阅者
	mu          sync.RWMutex
	subscribers map[string]map[chan protocol.TaskEvent]struct{}
}

var _ protocol.Scheduler = (*SQLiteScheduler)(nil)

func NewSQLiteScheduler(store protocol.Store) *SQLiteScheduler {
	return &SQLiteScheduler{
		store:       store,
		subscribers: make(map[string]map[chan protocol.TaskEvent]struct{}),
	}
}

func (s *SQLiteScheduler) Submit(ctx context.Context, task protocol.Task) (string, error) {
	if task.ID == "" {
		task.ID = fmt.Sprintf("task_%d", time.Now().UnixNano())
	}
	key := []byte("scheduler:task:" + task.ID)
	data, err := json.Marshal(task)
	if err != nil {
		return "", err
	}
	if err := s.store.Put(ctx, key, data); err != nil {
		return "", err
	}

	// 发出 submit 事件
	s.publish(task.ID, protocol.TaskEvent{
		TaskID: task.ID,
		State:  "submitted",
	})

	return task.ID, nil
}

func (s *SQLiteScheduler) Get(ctx context.Context, id string) (*protocol.Task, error) {
	key := []byte("scheduler:task:" + id)
	data, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	var task protocol.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (s *SQLiteScheduler) Cancel(ctx context.Context, id string) error {
	// MVP: 删除记录并通知 cancel 事件
	key := []byte("scheduler:task:" + id)
	err := s.store.Delete(ctx, key)
	if err == nil {
		s.publish(id, protocol.TaskEvent{
			TaskID: id,
			State:  "cancelled",
		})
	}
	return err
}

func (s *SQLiteScheduler) Subscribe(ctx context.Context, taskID string) (<-chan protocol.TaskEvent, error) {
	ch := make(chan protocol.TaskEvent, 16)

	s.mu.Lock()
	if s.subscribers[taskID] == nil {
		s.subscribers[taskID] = make(map[chan protocol.TaskEvent]struct{})
	}
	s.subscribers[taskID][ch] = struct{}{}
	s.mu.Unlock()

	// 清理逻辑
	go func() {
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
	}()

	return ch, nil
}

func (s *SQLiteScheduler) publish(taskID string, ev protocol.TaskEvent) {
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
