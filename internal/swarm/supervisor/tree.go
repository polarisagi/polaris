package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// RestartPolicy 定义了崩溃重启策略。
type RestartPolicy int

const (
	// OneForOne 策略：只重启崩溃的 Agent (默认)
	OneForOne RestartPolicy = iota
	// OneForAll 策略：一个崩溃，全部重启 (此处预留概念)
	OneForAll
)

// WorkerFunc 是 Agent 的主循环逻辑。
type WorkerFunc func(ctx context.Context) error

// WorkerEntry 记录注册到 Supervisor 中的 Worker 信息。
type WorkerEntry struct {
	ID           string
	Fn           WorkerFunc
	RestartCount int
	LastError    error
	LastCrashAt  time.Time
}

// Supervisor 管理和监督子协程（Agents）。
type Supervisor struct {
	mu          sync.Mutex
	workers     map[string]*WorkerEntry
	policy      RestartPolicy
	maxRestarts int
	timeWindow  time.Duration
	baseBackoff time.Duration
	maxBackoff  time.Duration
	wg          sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
}

// NewSupervisor 创建一个新的 Supervisor 引擎。
// 架构要求指数退避：100ms → 30s。
func NewSupervisor(maxRestarts int, timeWindow time.Duration) *Supervisor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Supervisor{
		workers:     make(map[string]*WorkerEntry),
		policy:      OneForOne,
		maxRestarts: maxRestarts,
		timeWindow:  timeWindow,
		baseBackoff: 100 * time.Millisecond,
		maxBackoff:  30 * time.Second,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// AddWorker 将 Agent 添加到监督树。
func (s *Supervisor) AddWorker(id string, fn WorkerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workers[id] = &WorkerEntry{
		ID: id,
		Fn: fn,
	}
}

// Start 启动监督树下的所有 Agent 并在后台监控。
func (s *Supervisor) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, w := range s.workers {
		s.wg.Add(1)
		concurrent.SafeGo(s.ctx, "supervisor.tree.worker", func(context.Context) {
			s.runWorker(w)
		})
	}
}

// Stop 停止所有监控的 Agent。
func (s *Supervisor) Stop() {
	s.cancel()
	s.wg.Wait()
}

// Wait 阻塞直到所有 worker 被优雅终止或由于超过最大重启次数而终止。
func (s *Supervisor) Wait() {
	s.wg.Wait()
}

// runWorker 内部方法，运行 Worker 并捕获 panic/错误以进行重启策略。
func (s *Supervisor) runWorker(w *WorkerEntry) {
	defer s.wg.Done()

	for {
		// 检查上下文是否已被取消
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		// 运行
		err := s.executeSafely(w)

		// 正常退出或上下文取消
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}

		// 发生崩溃，评估重启
		now := time.Now()

		s.mu.Lock()
		// 如果不在时间窗口内，则重置计数
		if now.Sub(w.LastCrashAt) > s.timeWindow {
			w.RestartCount = 0
		}

		w.LastError = err
		w.LastCrashAt = now
		w.RestartCount++

		currentCount := w.RestartCount
		s.mu.Unlock()

		if currentCount > s.maxRestarts {
			slog.Error("supervisor: worker exceeded max restarts, escalating", "worker", w.ID, "max_restarts", s.maxRestarts, "err", apperr.New(apperr.CodeInternal, "log event"))
			// 超出重启上限，退出。如果有父 Supervisor，这里可以 Escalate。
			return
		}

		// 指数退避: 100ms * 2^(count-1)
		backoff := time.Duration(float64(s.baseBackoff) * math.Pow(2, float64(currentCount-1)))
		if backoff > s.maxBackoff {
			backoff = s.maxBackoff
		}

		slog.Warn("supervisor: worker crashed, restarting", "worker", w.ID, "err", err, "backoff", backoff)

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
			// 继续循环，重新拉起
		}
	}
}

func (s *Supervisor) executeSafely(w *WorkerEntry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = apperr.New(apperr.CodeInternal, fmt.Sprintf("panic: %v", r))
		}
	}()
	err = w.Fn(s.ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Supervisor.executeSafely", err)
	}
	return nil
}
