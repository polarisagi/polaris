package swarm

import (
	"context"
	"fmt"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// 编排模式实现。
// 架构文档: docs/arch/08-Multi-Agent-Orchestrator-深度选型.md §3, §1.3-1.9

// OrchestrationMode 编排模式。
type OrchestrationMode int

const (
	ModeSupervisor OrchestrationMode = iota // 默认: Planner→Worker→汇总
	ModeHierarchy                           // 递归分解
	ModeSequential                          // A输出→B输入
	ModeParallel                            // 独立子任务并发
	ModeMapReduce                           // 分片归并
	ModeReflection                          // 执行→审查→改进
	ModeSwarm                               // 去中心化handoff
)

// PhasedStartup 分阶段启动（拓扑排序确保被依赖 Agent 先于消费者启动）。
// 1. DependencyGraph: 解析 I/O 声明, DFS 三色环路检测
// 2. TopologicalSort: Kahn 入度分层
// 3. PhasedStart: P0 [M11] Policy+Cedar-Gate + Blackboard+SQLite → P1 M5+M10 索引 → P2 M6+M7 注册 → P3 Orchestrator+Planner → P4 Worker/Reviewer
// 4. HealthCheckGate: 每层 30s 超时 → ErrPhaseStartupTimeout
type PhasedStartup struct {
	phases []StartupPhase
}

// StartupPhase 启动阶段。
type StartupPhase struct {
	Name       string
	Components []string
	Timeout    int // 30s
}

// Start 执行分阶段启动。
func (ps *PhasedStartup) Start(ctx context.Context) error {
	for _, phase := range ps.phases {
		if err := ps.startPhase(ctx, phase); err != nil {
			return perrors.New(perrors.CodeInternal, fmt.Sprintf("startup phase %q failed: %v", phase.Name, err))
		}
	}
	return nil
}

func (ps *PhasedStartup) startPhase(ctx context.Context, phase StartupPhase) error {
	// 应用 30s 熔断超时
	timeout := time.Duration(phase.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	phaseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 模拟健康检查：并发启动各个 Component 并等待就绪
	errCh := make(chan error, len(phase.Components))
	var wg sync.WaitGroup

	for _, comp := range phase.Components {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()

			// 模拟各组件启动，实际实现中应调用各 Component 的 Init() / Start()
			// 如果在 30s 内未就绪或发生 Panic，应返回 error
			select {
			case <-time.After(10 * time.Millisecond): // 模拟快速启动
				// success
			case <-phaseCtx.Done():
				errCh <- perrors.New(perrors.CodeInternal, fmt.Sprintf("component %s timeout", c))
			}
		}(comp)
	}

	// 等待所有组件完成或 ctx 超时
	go func() {
		wg.Wait()
		close(errCh)
	}()

	for err := range errCh {
		if err != nil {
			return err
		}
	}

	return phaseCtx.Err()
}

// TopologyEvolver 编排拓扑自演化。
// Evaluate: 获取候选 fitness → Pareto 前沿 (成功率 × token 效率)
// → 候选 ≥ 当前 5pp → TopologyChange (A/B 50/50 分流 50 任务).
type TopologyEvolver struct {
	fitnessMap map[string]*TopologyFitness
}

// Evaluate 评估候选拓扑：Pareto 前沿（成功率 × token 效率）双维度比较。
// SampleSize < 10 的候选不参与评估，防冷启动噪音。
// 候选在成功率上领先 baseline ≥5pp 且 token 效率不劣化（cost ≤ base×1.1）时返回 true。
func (te *TopologyEvolver) Evaluate(candidate *TopologyFitness, baseline string) bool {
	if te.fitnessMap == nil {
		te.fitnessMap = make(map[string]*TopologyFitness)
	}
	te.fitnessMap[candidate.Topology] = candidate
	if candidate.SampleSize < 10 {
		return false // 样本不足，不参与评估
	}
	base, ok := te.fitnessMap[baseline]
	if !ok {
		return true // 无基线，接受新候选
	}
	if base.SampleSize < 10 {
		return true // 基线样本不足，候选直接接受
	}
	// Pareto 双维：成功率领先 ≥5pp 且 token 成本不劣化超 10%
	successLead := candidate.SuccessRate >= base.SuccessRate+0.05
	tokenOK := base.AvgTokenCost == 0 || candidate.AvgTokenCost <= base.AvgTokenCost*1.1
	return successLead && tokenOK
}

// TopologyFitness 拓扑适应度。
type TopologyFitness struct {
	Topology         string
	TaskType         string
	SuccessRate      float64
	AvgLatencyMs     int64
	AvgTokenCost     float64
	AgentUtilization float64 // 0-1，单任务内 Agent 活跃占比
	SampleSize       int     // <10 不参与 Pareto 评估
}
