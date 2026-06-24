// Package scheduler 提供 M13 任务调度的正式实现。
// 权威接口: internal/protocol/interfaces.go (protocol.Scheduler / protocol.HITL)
// 正式实现: SQLiteScheduler (queue.go) 实现 protocol.Scheduler
//
//	GatewayImpl    (../hitl/gateway.go) 实现 protocol.HITL
//
// 架构文档: docs/arch/M13-Interface-Scheduler.md §2
package automation

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// cpuSampler 读取 /proc/stat 计算真实 CPU 占用率（M13 §3 ResourceGovernor）。
// 非 Linux 平台降级为 goroutine 数量启发式（只改 cpuProbeFn，接口不变）。
// 采样结果缓存 1s，避免高频 syscall 开销。
type cpuSampler struct {
	mu        sync.Mutex
	lastIdle  uint64
	lastTotal uint64
	lastTime  time.Time
	lastPct   float64
}

// Usage 返回系统 CPU 占用率百分比（0–100）。
func (cs *cpuSampler) Usage() float64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if time.Since(cs.lastTime) < time.Second && cs.lastTotal > 0 {
		return cs.lastPct
	}

	idle, total, err := readProcStatCPU()
	if err != nil {
		// 非 Linux 或 /proc/stat 不可读：降级为 goroutine 启发式
		g := runtime.NumGoroutine()
		switch {
		case g > 100:
			return 80.0
		case g > 50:
			return 50.0
		default:
			return 20.0
		}
	}

	if cs.lastTotal > 0 && total > cs.lastTotal {
		dTotal := total - cs.lastTotal
		dIdle := idle - cs.lastIdle
		if dTotal > 0 {
			cs.lastPct = float64(dTotal-dIdle) / float64(dTotal) * 100
		}
	}
	cs.lastIdle = idle
	cs.lastTotal = total
	cs.lastTime = time.Now()
	return cs.lastPct
}

// readProcStatCPU 解析 /proc/stat 第一行，返回 (idle, total) CPU 时钟滴答数。
// 格式：cpu user nice system idle iowait irq softirq steal ...
func readProcStatCPU() (idle, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, apperr.Wrap(apperr.CodeInternal, "readProcStatCPU", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0, apperr.New(apperr.CodeInternal, "scheduler/cpu: empty /proc/stat")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, apperr.New(apperr.CodeInternal, "scheduler/cpu: unexpected /proc/stat format")
	}

	for i, f := range fields[1:] {
		v, e := strconv.ParseUint(f, 10, 64)
		if e != nil {
			continue
		}
		total += v
		if i == 3 { // index 3 (fields[4]) = idle
			idle = v
		}
	}
	return idle, total, nil
}

// TaskStatus 任务生命周期枚举。
// 与 types.Task.Status 对齐。
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

// ScheduledTask 调度任务（Cron 定时 + 一次性）。
// 面向 Cron/周期调度场景；即席任务使用 types.Task 通过 SQLiteScheduler 提交。
type ScheduledTask struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CronExpr  string     `json:"cron_expr,omitempty"`
	CronTZ    string     `json:"cron_tz,omitempty"`    // 时区（空值 = "UTC"）
	StaggerMs int        `json:"stagger_ms,omitempty"` // 执行前随机抖动毫秒（防雷群）
	Status    TaskStatus `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	LastRun   time.Time  `json:"last_run,omitzero"`
	NextRun   time.Time  `json:"next_run,omitzero"`

	// 失败隔离（连续错误超阈值自动禁用）
	ConsecutiveErrors int        `json:"consecutive_errors,omitzero"`
	DisabledAt        *time.Time `json:"disabled_at,omitzero"`
}

// ResourceGovernor 全局资源入场决策——三级降级保护。
// 与 M13 §3 ResourceGovernor 对齐。
type ResourceGovernor struct {
	mu            sync.Mutex
	cond          *sync.Cond
	maxConcurrent int
	inFlight      int
	cfg           config.ResourceGovernorConfig

	awake int32

	memProbeFn func() (freeMB int64)
	cpuProbeFn func() (usage float64)
}

func NewResourceGovernor(maxConcurrent int, cfg config.ResourceGovernorConfig) *ResourceGovernor {
	if cfg.MemL1FreeMB == 0 {
		cfg.MemL1FreeMB = 1024
		cfg.MemL2FreeMB = 512
		cfg.MemL3FreeMB = 256
		cfg.CPUL1Pct = 70.0
		cfg.CPUL2Pct = 90.0
	}
	cpu := &cpuSampler{}
	rg := &ResourceGovernor{
		maxConcurrent: maxConcurrent,
		cfg:           cfg,
		memProbeFn: func() int64 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return int64(m.Sys-m.HeapAlloc) / (1024 * 1024)
		},
		// 使用 /proc/stat 真实 CPU 占用率；非 Linux 自动降级为 goroutine 启发式。
		cpuProbeFn: cpu.Usage,
	}
	rg.cond = sync.NewCond(&rg.mu)
	return rg
}

// interactiveConcurrencyMultiplier 交互式任务（priority=0）允许超过 maxConcurrent 的倍数上限。
// 防止 priority=0 任务无界堆积导致 OOM，同时保留其优先准入语义。
const interactiveConcurrencyMultiplier = 4

// Admit implements §2.0 3-level degradation rule:
func (rg *ResourceGovernor) Admit(priority int) (bool, int) {
	rg.mu.Lock()
	defer rg.mu.Unlock()

	freeMemMB := rg.memProbeFn()
	cpuUsage := rg.cpuProbeFn()
	degradeLevel := 0

	if freeMemMB < int64(rg.cfg.MemL1FreeMB) || cpuUsage > rg.cfg.CPUL1Pct {
		degradeLevel = 1
	}
	if freeMemMB < int64(rg.cfg.MemL2FreeMB) || cpuUsage > rg.cfg.CPUL2Pct {
		degradeLevel = 2
	}

	if freeMemMB < int64(rg.cfg.MemL3FreeMB) && priority != 0 {
		return false, 3
	}
	if freeMemMB < int64(rg.cfg.MemL3FreeMB) {
		degradeLevel = 3
	}
	if (cpuUsage > rg.cfg.CPUL1Pct || freeMemMB < int64(rg.cfg.MemL2FreeMB)) && priority != 0 {
		return false, 2
	}

	if priority == 0 {
		hardCap := rg.maxConcurrent * interactiveConcurrencyMultiplier
		if rg.inFlight >= hardCap {
			return false, degradeLevel
		}
		rg.inFlight++
		return true, degradeLevel
	}

	if rg.inFlight >= rg.maxConcurrent {
		return false, degradeLevel
	}

	rg.inFlight++
	return true, degradeLevel
}

// InFlight 返回当前进行中的任务数。
func (rg *ResourceGovernor) InFlight() int {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.inFlight
}

// WaitForCapacity 阻塞直到容量释放或上下文取消（sync.Cond，零忙等待）。
func (rg *ResourceGovernor) WaitForCapacity(ctx context.Context) error {
	// 用 channel 将 ctx 取消信号与 cond.Wait 解耦
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			rg.cond.Broadcast() // 唤醒所有等待者，让它们检查 ctx
		case <-stop:
		}
	}()
	defer close(stop)

	rg.mu.Lock()
	defer rg.mu.Unlock()
	for rg.inFlight >= rg.maxConcurrent {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rg.cond.Wait()
	}
	return ctx.Err()
}

func (rg *ResourceGovernor) Release() {
	rg.mu.Lock()
	rg.inFlight--
	rg.cond.Signal()
	rg.mu.Unlock()
}

func (rg *ResourceGovernor) WakeUp() {
	if atomic.CompareAndSwapInt32(&rg.awake, 0, 1) {
		rg.cond.Broadcast()
	}
}

// HITLCheckpoint HITL 审批点（供 CronJob 注入审批等待）。
// 正式 HITL 接口见 internal/protocol/interfaces.go:HITL。
type HITLCheckpoint struct {
	CheckpointID string
	Timeout      time.Duration
}

func (c *HITLCheckpoint) AwaitApproval(ctx context.Context) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(c.Timeout):
		return false, apperr.New(apperr.CodeTimeout, "hitl: approval timeout") // 超时视为拒绝
	}
}

// Scheduler 定时/异步任务调度执行器。
type Scheduler struct {
	invoker protocol.AgentInvoker
	gate    backgroundGate
}

type backgroundGate interface {
	BackgroundPermit(priority int) bool
}

func (s *Scheduler) WithBackgroundGate(g backgroundGate) { s.gate = g }

func NewScheduler(invoker protocol.AgentInvoker) *Scheduler {
	return &Scheduler{invoker: invoker}
}

func (s *Scheduler) ExecuteTask(ctx context.Context, task *types.Task) error {
	if task != nil && task.Priority > 1 {
		if s.gate != nil && !s.gate.BackgroundPermit(task.Priority) {
			return nil // 跳过本轮
		}
	}
	if s.invoker != nil && task != nil && task.Type == "agent" {
		_, err := s.invoker.InvokeAgent(ctx, string(task.Payload))
		return err
	}
	// 支持原本的 tool 或 script 执行逻辑
	return nil
}
