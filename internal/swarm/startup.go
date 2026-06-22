package swarm

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

// Pinger 是每个子系统的健康检查接口（consumer-side）。
// 每个组件在其自身包内实现；这里仅声明接口，防止包循环。
type Pinger interface {
	Ping(ctx context.Context) error
}

// Phase 启动阶段编号。
type Phase int

const (
	PhasePolicy      Phase = 0 // P0: Cedar Gate + Blackboard
	PhaseMemory      Phase = 1 // P1: Memory + Knowledge
	PhaseExtensions  Phase = 2 // P2: Skill + MCP
	PhaseOrchestrate Phase = 3 // P3: Orchestrator + Planner
	PhaseWorkers     Phase = 4 // P4: Worker + Reviewer
)

const healthCheckGateTimeout = 30 * time.Second

// PhaseEntry 描述一个启动阶段。
type PhaseEntry struct {
	Phase   Phase
	Name    string
	Pingers []Pinger
	// OnFail 控制失败策略：P0 失败必须 panic（策略真空不可接受），P1-P4 返回 error。
	OnFail func(phase Phase, err error)
}

// PhasedStartup 实现 M08 §1.9 分阶段启动序列。
type PhasedStartup struct {
	phases []PhaseEntry
}

// NewPhasedStartup 构建启动序列。调用者按 P0→P4 顺序追加 PhaseEntry。
func NewPhasedStartup(phases []PhaseEntry) *PhasedStartup {
	return &PhasedStartup{phases: phases}
}

// Run 依序执行各阶段，每阶段有 30s HealthCheckGate。
// P0 失败调用 OnFail（通常 panic）；P1+ 失败返回 error 供调用方决策。
func (ps *PhasedStartup) Run(ctx context.Context) error {
	for _, entry := range ps.phases {
		phaseCtx, cancel := context.WithTimeout(ctx, healthCheckGateTimeout)

		g, gCtx := errgroup.WithContext(phaseCtx)
		for _, p := range entry.Pingers {
			pinger := p
			g.Go(func() error {
				return pinger.Ping(gCtx)
			})
		}

		err := g.Wait()
		cancel()

		if err != nil {
			if entry.OnFail != nil {
				entry.OnFail(entry.Phase, err)
			}
			return fmt.Errorf("PhasedStartup.Run: %w", err)
		}
		slog.Info("startup: phase complete", "phase", entry.Name)
	}
	return nil
}
