// Package scheduler 提供 M13 任务调度的正式实现。
// 权威接口: internal/protocol/（按域拆分的 interfaces_*.go） (protocol.Scheduler / protocol.HITL)
// 正式实现: SQLiteScheduler (queue.go) 实现 protocol.Scheduler
//
//	GatewayImpl    (../hitl/gateway.go) 实现 protocol.HITL
//
// 架构文档: docs/arch/M13-Interface-Scheduler.md §2
package automation

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// backgroundAdmissionTotal 后台工作准入结果计数（HE-1：限流必须可观测，
// 否则"后台任务为什么没跑"只能靠猜）。status: admitted / denied_pressure / denied_busy。
//
// custom-nolint:global-var
//
//nolint:gochecknoglobals // Prometheus 指标，与 idleEvolutionTasksTotal 同属一等公民（ADR-0001）
var backgroundAdmissionTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "polaris_background_admission_total",
		Help: "Background work admission decisions by ResourceGovernor.",
	},
	[]string{"work", "status"},
)

// CPU 与内存探针已于 2026-09-22 归口到 internal/observability/probe
// （CLAUDE.md：probe/ = 硬件与内存探针）。本文件原有的私有 cpuSampler 只实现了
// Linux 的 /proc/stat，其余平台降级成"goroutine 数量启发式"——Polaris 常驻
// goroutine 远超 100，于是 macOS/Windows 上恒定返回 80.0，与判据
// `cpuUsage > cpu_l1_pct(80.0)` 擦边而过，纯属侥幸；现由 probe 包提供三平台
// 真实实现（linux /proc/stat 增量、darwin vm.loadavg、windows GetSystemTimes）。

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

	// LLM 专属并发限流 (P0-3)
	maxConcurrentLLMCalls int
	llmInFlight           int

	cfg config.ResourceGovernorConfig

	memProbeFn       func() (freeMB int64)
	cpuProbeFn       func() (usage float64)
	activityCallback func()
}

// NewResourceGovernor 创建并初始化全局资源治理器。
// maxConcurrent 定义全局最大并发任务数，cfg 提供多级降级的水位线配置。
func NewResourceGovernor(maxConcurrent int, cfg config.ResourceGovernorConfig) *ResourceGovernor {
	if cfg.MemL1FreeMB == 0 {
		cfg.MemL1FreeMB = 1024
		cfg.MemL2FreeMB = 512
		cfg.MemL3FreeMB = 256
		cfg.CPUL1Pct = 70.0
		cfg.CPUL2Pct = 90.0
	}
	rg := &ResourceGovernor{
		maxConcurrent: maxConcurrent,
		cfg:           cfg,
		// [2026-09-22 根因修复] 原实现返回的是 `m.Sys - m.HeapAlloc`——Go 运行时
		// 自己向 OS 要到、但当前不在存活堆对象里的字节数，与"系统还剩多少可用
		// 内存"毫无关系，量级只有几百 MB 且随 GC 时机剧烈抖动。它被拿去和
		// mem_l2_free_mb=1024 / mem_l3_free_mb=512 这两个**系统级** MB 阈值比较，
		// 于是 AdmitLLM 的降级闸门按 GC 节奏随机误触发：一旦落到阈值以下，所有
		// priority != 0 的推理（而交互式对话是唯一调用方，恒传 1）被直接拒绝，
		// 表现为用户侧 60 秒后拿到一条无法归因的"推理返回空内容"。
		// 改用 probe 包的平台原生探针（darwin/linux 各自实现，启动日志里
		// "AutoConfig: ... ram=16384MB(avail=6553MB)" 用的就是它），
		// 其内部已含探测失败时的保守兜底，不需要在这里再造一份。
		memProbeFn: func() int64 {
			return int64(probe.ProbeAvailableMemoryMB())
		},
		cpuProbeFn: probe.NewCPUSampler().Usage,
	}
	rg.cond = sync.NewCond(&rg.mu)
	return rg
}

// WithMaxConcurrentLLM 注入 LLM 的并发上限
func (rg *ResourceGovernor) WithMaxConcurrentLLM(n int) *ResourceGovernor {
	rg.maxConcurrentLLMCalls = n
	if rg.maxConcurrentLLMCalls == 0 {
		rg.maxConcurrentLLMCalls = 4 // 默认 fallback
	}
	return rg
}

// OnActivity 注册活跃事件回调
func (rg *ResourceGovernor) OnActivity(cb func()) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	rg.activityCallback = cb
}

// interactiveConcurrencyMultiplier 交互式任务（priority=0）允许超过 maxConcurrent 的倍数上限。
// 防止 priority=0 任务无界堆积导致 OOM，同时保留其优先准入语义。
const interactiveConcurrencyMultiplier = 4

// Admit 实现 §2.0 三级降级策略。
// 评估当前可用内存和 CPU 使用率，返回是否准入 (admitted) 以及触发的降级级别 (degradeLevel, 0-3)。
// priority=0 表示交互式高优任务，允许突破一般并发上限。
func (rg *ResourceGovernor) Admit(priority int) (bool, int) {
	rg.mu.Lock()
	if rg.activityCallback != nil {
		rg.activityCallback()
	}
	defer rg.mu.Unlock()

	deny, degradeLevel := rg.denyDegradableLocked()
	if deny && priority != 0 {
		return false, degradeLevel
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

// AdmitBackground 后台工作准入闸门：周期性重索引、向量回填、知识同步、记忆巩固
// 等"可以下次再跑"的工作在每轮开始前调用，拿到 release 才执行，用完必须调用
// release（惯用 `defer release()`）。
//
// [2026-09-22 接线] 此前 ResourceGovernor 只有 Admit/AdmitLLM 两个入口，而 Admit
// 全仓无人调用——真实效果是"该被限流的后台任务完全不受限，该被保活的用户对话
// 反而被内存/CPU 闸门拦死"，设计意图整个反了。实测表现：清库重启后，148 个扩展
// 的向量回填 + STT 模型下载 + 知识连接器全量同步同时抢占本地嵌入引擎，交互式
// 检索（Knowledge.Search）连续 30 秒超时。
//
// 与 Admit / AdmitLLM 的三点差异，都源于后台工作与用户请求不同的语义：
//  1. **不触发 activityCallback**。活跃标记的含义是"用户在用系统"，供
//     IdleEvolutionScheduler 判定空闲窗口；后台工作自己打这个标记，会把空闲窗口
//     永久顶掉，自进化从此再也不会启动。
//  2. **拿不到额度直接返回 false，不排队**。后台工作的语义是"这轮跳过，下个
//     tick 再来"；排队只会让压力解除的瞬间所有积压任务一次性涌出，再次压垮系统。
//  3. **判据用 denyDegradableLocked**，即内存低于 L2 或 CPU 超过 L1 即拒，
//     对齐配置里"L2 阻塞：挂起所有后台任务""CPU 阈值：降低后台任务优先级"。
func (rg *ResourceGovernor) AdmitBackground(name string) (release func(), admitted bool) {
	// nil 接收者保护：本方法是以**接口**形式注入各后台组件的（如
	// connector.BackgroundAdmitter），而 boot 层的 sb.ResourceGov 允许为 nil。
	// 一个 nil 的 *ResourceGovernor 装进接口后接口本身非 nil，调用方的
	// `if s.admitter != nil` 拦不住——不在这里兜住就是一次空指针 panic。
	// 治理器缺席时放行，保持"治理是增强而非前置依赖"。
	if rg == nil {
		return func() {}, true
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()

	deny, degradeLevel := rg.denyDegradableLocked()
	if deny {
		backgroundAdmissionTotal.WithLabelValues(name, "denied_pressure").Inc()
		slog.Debug("resource_governor: background work skipped under pressure",
			"work", name, "degrade_level", degradeLevel)
		return nil, false
	}
	if rg.maxConcurrent > 0 && rg.inFlight >= rg.maxConcurrent {
		backgroundAdmissionTotal.WithLabelValues(name, "denied_busy").Inc()
		return nil, false
	}

	rg.inFlight++
	backgroundAdmissionTotal.WithLabelValues(name, "admitted").Inc()

	var once sync.Once
	return func() { once.Do(rg.Release) }, true
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
	concurrent.SafeGo(ctx, "automation.resource_governor.wait_capacity", func(context.Context) {
		select {
		case <-ctx.Done():
			rg.cond.Broadcast() // 唤醒所有等待者，让它们检查 ctx
		case <-stop:
		}
	})
	defer close(stop)

	rg.mu.Lock()
	defer rg.mu.Unlock()
	for rg.inFlight >= rg.maxConcurrent {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
		}
		rg.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
	}
	return nil
}

// Release 释放一个并发额度，唤醒等待队列中的下一个任务。
func (rg *ResourceGovernor) Release() {
	rg.mu.Lock()
	rg.inFlight--
	rg.cond.Broadcast() // GR-10.1-004：任务池与 LLM 池共用一个 cond，Signal 可能唤醒另一池的等待者而丢失唤醒
	rg.mu.Unlock()
}

// AdmitLLM 为 LLM 请求分配并发额度，并回报当前资源降级等级。
//
// priority 语义（2026-09-22 重新定义，见下）：
//   - 0：用户可见推理（交互式对话）。**只受并发上限约束**，内存/CPU 水位线
//     只影响回报的 degradeLevel，不构成拒绝理由。
//   - ≥1：可降级的后台推理（自进化、批量提炼等）。保留三级水位线闸门。
//
// 为什么用户可见推理不再受内存/CPU 闸门约束：
//
//	(1) 拒绝一次**远程** LLM 调用并不能缓解本机内存压力——它消耗的是一个 socket
//	    与几百 KB 流式缓冲，真正吃内存的是本地模型权重、沙箱与后台任务。用内存
//	    水位线去拦远程 API 调用，付出的是"产品核心功能不可用"，换回的是几乎为零
//	    的内存回收。
//	(2) 本地推理确实吃内存，但它有自己的、更精确的治理链路：FeatureGate 按 Tier
//	    决定是否解锁本地推理、AutoConfig 内存压力回调驱动 local model unloader
//	    卸载权重。重复在这里加一道粗粒度闸门只会误伤远程调用。
//	(3) 旧语义在实现层面本就是空转：AdmitLLM 全仓唯一调用方是
//	    InferenceRouter.acquireLLMCapacity，且恒传 priority=1，而两道闸门都是
//	    `&& priority != 0` 才生效——等于"交互式对话"是唯一被拦的对象，而本该被
//	    限流的后台任务走的是 Admit()，那个方法至今无人调用。设计意图整个反了。
//	(4) 被拒时的表现也不可接受：WaitForLLMCapacity 只等 llmInFlight，压根不等内存
//	    恢复，于是必然在 60 秒后被同一道闸门再拒一次，用户侧只看到一句"推理返回
//	    空内容"。等待机制与拒绝理由根本不匹配。
//
// 内存真正见底时的正确降级顺序是"先停后台自进化、再停本地模型、最后才是对话"，
// 而不是反过来先掐对话。degradeLevel 照常回报，供调用方做质量降级（缩短上下文、
// 关闭并行工具调用等），这才是这个信号该有的用法。
func (rg *ResourceGovernor) AdmitLLM(priority int) (bool, int) {
	rg.mu.Lock()
	if rg.activityCallback != nil {
		rg.activityCallback()
	}
	defer rg.mu.Unlock()

	deny, degradeLevel := rg.denyDegradableLocked()

	// 后台可降级推理：维持水位线闸门。
	if priority != 0 && deny {
		return false, degradeLevel
	}

	// 并发上限对所有优先级一视同仁——它是吞吐与公平性控制，且有配套的
	// WaitForLLMCapacity 等待机制，拒绝后调用方能真正等到额度释放。
	if rg.maxConcurrentLLMCalls > 0 && rg.llmInFlight >= rg.maxConcurrentLLMCalls {
		return false, degradeLevel
	}

	rg.llmInFlight++
	return true, degradeLevel
}

// denyDegradableLocked 判定"可降级工作"（后台任务 / 后台推理）当前是否应被拒绝，
// 并一并返回降级等级。调用方需持有 rg.mu。
//
// 判据逐条对齐 configs/defaults.toml [system.resource_governor] 对各水位线的定义，
// 不再凭等级序号隐式推导——那正是旧实现把"降低后台任务优先级"的 CPU 阈值
// 套到用户对话上的原因：
//
//	mem_l3_free_mb "L3 濒死：强杀耗存最大的子进程" → 必拒
//	mem_l2_free_mb "L2 阻塞：挂起所有后台任务"     → 内存低于 L2 即拒
//	cpu_l1_pct     "CPU 阈值：降低后台任务优先级"  → CPU 超过 L1 即拒
//	mem_l1_free_mb "L1 警告：停止拉起新 Agent"     → 仅计入等级，不单独拒绝
func (rg *ResourceGovernor) denyDegradableLocked() (deny bool, degradeLevel int) {
	freeMemMB := rg.memProbeFn()
	cpuUsage := rg.cpuProbeFn()

	switch {
	case freeMemMB < int64(rg.cfg.MemL3FreeMB):
		degradeLevel = 3
	case freeMemMB < int64(rg.cfg.MemL2FreeMB) || cpuUsage > rg.cfg.CPUL2Pct:
		degradeLevel = 2
	case freeMemMB < int64(rg.cfg.MemL1FreeMB) || cpuUsage > rg.cfg.CPUL1Pct:
		degradeLevel = 1
	}

	deny = degradeLevel >= 2 || cpuUsage > rg.cfg.CPUL1Pct
	return deny, degradeLevel
}

// WaitForLLMCapacity 阻塞直到 LLM 容量释放或上下文取消
func (rg *ResourceGovernor) WaitForLLMCapacity(ctx context.Context) error {
	stop := make(chan struct{})
	concurrent.SafeGo(ctx, "automation.resource_governor.wait_llm_capacity", func(context.Context) {
		select {
		case <-ctx.Done():
			rg.cond.Broadcast()
		case <-stop:
		}
	})
	defer close(stop)

	rg.mu.Lock()
	defer rg.mu.Unlock()
	for rg.maxConcurrentLLMCalls > 0 && rg.llmInFlight >= rg.maxConcurrentLLMCalls {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
		}
		rg.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
	}
	return nil
}

// ReleaseLLM 释放 LLM 的并发额度
func (rg *ResourceGovernor) ReleaseLLM() {
	rg.mu.Lock()
	rg.llmInFlight--
	rg.cond.Broadcast() // GR-10.1-004：任务池与 LLM 池共用一个 cond，Signal 可能唤醒另一池的等待者而丢失唤醒
	rg.mu.Unlock()
}
