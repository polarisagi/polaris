package topology

import (
	"log/slog"
	"sync"
	"time"
)

// TrafficSplitter 定义分流器接口（打破 L2->L3 依赖）。
type TrafficSplitter interface {
	SetPercent(percent int)
	Route(sessionID string) string
	Rollback()
}

// EvolverPhase 拓扑自演化阶段状态机。
// Shadow(0%) → AB(50%, 50任务) → Gradual(100%, 7d) → Commit
type EvolverPhase int

const (
	EvolverPhaseIdle    EvolverPhase = iota // 无候选
	EvolverPhaseShadow                      // 0% 流量，观察 50 任务
	EvolverPhaseAB                          // 50% 流量，观察 50 任务
	EvolverPhaseGradual                     // 100% 流量，观察 7d
	EvolverPhaseCommit                      // 永久切换
)

// TopologyEvolverService 封装 TopologyEvolver + TrafficSplitter，驱动状态机。
// 不独立持有 goroutine；由 Orchestrator 在任务完成后调用 RecordOutcome。
type TopologyEvolverService struct {
	mu        sync.Mutex
	evolver   *TopologyEvolver
	splitter  TrafficSplitter
	baseline  string // 当前基线拓扑
	candidate string // 当前候选拓扑（"" = 无）
	phase     EvolverPhase
	// AB 阶段计数
	abTasksDone int
	abSuccess   int
	abBaseline  float64 // AB 开始时的基线成功率快照

	// Gradual 阶段计时
	gradualStart time.Time
}

// NewTopologyEvolverService 创建服务，baseline 为初始拓扑名称（如 "supervisor"）。
func NewTopologyEvolverService(baseline string, splitter TrafficSplitter) *TopologyEvolverService {
	return &TopologyEvolverService{
		evolver:  &TopologyEvolver{},
		splitter: splitter,
		baseline: baseline,
		phase:    EvolverPhaseIdle,
	}
}

// ProposeCandidateTopology 提出候选拓扑。仅当无活跃候选时生效。
// taskType 用于隔离不同任务类型的适应度数据。
func (s *TopologyEvolverService) ProposeCandidateTopology(candidateTopology string, baselineFitness *TopologyFitness) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != EvolverPhaseIdle {
		return // 已有进行中的演化
	}
	if baselineFitness == nil || baselineFitness.SampleSize < 50 {
		return // 基线样本不足（M08 §8 要求 ≥50 历史执行）
	}
	s.candidate = candidateTopology
	s.abBaseline = baselineFitness.SuccessRate
	s.phase = EvolverPhaseShadow
	if s.splitter != nil {
		s.splitter.SetPercent(0)
	}
	slog.Info("topology_evolver: shadow phase started",
		"baseline", s.baseline, "candidate", candidateTopology)
}

// RecordOutcome 在每个任务完成后调用，更新适应度并驱动状态机。
// topology：执行本任务的实际拓扑；success：任务是否成功；tokenCost：token 消耗。
//
//nolint:gocyclo
func (s *TopologyEvolverService) RecordOutcome(topology string, taskType string, success bool, tokenCost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fitness := &TopologyFitness{
		Topology:     topology,
		TaskType:     taskType,
		SuccessRate:  boolToFloat(success), // 单次记录，Evaluate 内部累积
		AvgTokenCost: tokenCost,
		SampleSize:   1,
	}
	s.evolver.RecordSample(fitness)

	switch s.phase {
	case EvolverPhaseShadow:
		// Shadow：累积 50 次候选观测，不切流量，仅评估
		candFitness := s.evolver.GetFitness(s.candidate, taskType)
		baseFitness := s.evolver.GetFitness(s.baseline, taskType)
		if candFitness != nil && candFitness.SampleSize >= 50 {
			if s.evolver.Evaluate(candFitness, s.baseline) {
				s.phase = EvolverPhaseAB
				s.abTasksDone = 0
				s.abSuccess = 0
				if s.splitter != nil {
					s.splitter.SetPercent(50)
				}
				slog.Info("topology_evolver: AB phase started", "candidate", s.candidate)
			} else {
				// 候选未通过 Shadow 评估，重置
				slog.Info("topology_evolver: shadow eval failed, resetting",
					"candidate", s.candidate,
					"candidate_rate", candFitness.SuccessRate,
					"baseline_rate", baseFitnessRate(baseFitness))
				s.resetLocked()
			}
		}

	case EvolverPhaseAB:
		if topology == s.candidate {
			s.abTasksDone++
			if success {
				s.abSuccess++
			}
		}
		if s.abTasksDone >= 50 {
			candidateRate := float64(s.abSuccess) / float64(s.abTasksDone)
			// 回滚条件：候选成功率低于基线 5pp 以上
			if s.abBaseline > 0 && candidateRate < s.abBaseline-0.05 {
				slog.Warn("topology_evolver: AB regression, rolling back",
					"candidate_rate", candidateRate, "baseline_rate", s.abBaseline)
				s.rollbackLocked()
				return
			}
			// 通过 AB，进入 Gradual（100% 切换）
			s.phase = EvolverPhaseGradual
			s.gradualStart = time.Now()
			if s.splitter != nil {
				s.splitter.SetPercent(100)
			}
			slog.Info("topology_evolver: gradual phase started (100%)", "candidate", s.candidate)
		}

	case EvolverPhaseGradual:
		// Gradual：持续观察 7d，检测退化
		candFitness := s.evolver.GetFitness(s.candidate, taskType)
		baseFitness := s.evolver.GetFitness(s.baseline, taskType)
		if candFitness != nil && baseFitness != nil {
			drop := baseFitness.SuccessRate - candFitness.SuccessRate
			if drop > 0.03 { // 退化 >3pp 回滚（M08 §8）
				slog.Warn("topology_evolver: gradual regression, rolling back",
					"drop_pp", drop*100)
				s.rollbackLocked()
				return
			}
		}
		if time.Since(s.gradualStart) >= 7*24*time.Hour {
			s.phase = EvolverPhaseCommit
			s.baseline = s.candidate
			s.candidate = ""
			slog.Info("topology_evolver: committed new baseline", "topology", s.baseline)
		}
	}
}

// RouteTopology 为新任务路由拓扑。AB 阶段使用 TrafficSplitter 分流。
func (s *TopologyEvolverService) RouteTopology(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == EvolverPhaseAB || s.phase == EvolverPhaseGradual {
		if s.splitter != nil {
			routed := s.splitter.Route(sessionID)
			if routed == s.candidate {
				return s.candidate
			}
		}
	}
	return s.baseline
}

func (s *TopologyEvolverService) rollbackLocked() {
	if s.splitter != nil {
		s.splitter.Rollback()
	}
	s.resetLocked()
}

func (s *TopologyEvolverService) resetLocked() {
	s.candidate = ""
	s.phase = EvolverPhaseIdle
	s.abTasksDone = 0
	s.abSuccess = 0
}

func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

func baseFitnessRate(f *TopologyFitness) float64 {
	if f == nil {
		return 0
	}
	return f.SuccessRate
}

// TopologyEvolver 评估候选拓扑适应度（Pareto 前沿）。
type TopologyEvolver struct {
	fitnessMap map[string]*TopologyFitness
}

// Evaluate 评估候选是否优于基线：成功率领先 ≥5pp 且 token 成本不劣化超 10%。
func (te *TopologyEvolver) Evaluate(candidate *TopologyFitness, baseline string) bool {
	if te.fitnessMap == nil {
		te.fitnessMap = make(map[string]*TopologyFitness)
	}
	key := candidate.Topology + "|" + candidate.TaskType
	te.fitnessMap[key] = candidate
	if candidate.SampleSize < 10 {
		return false
	}
	baseKey := baseline + "|" + candidate.TaskType
	base, ok := te.fitnessMap[baseKey]
	if !ok || base.SampleSize < 10 {
		return true
	}
	successLead := candidate.SuccessRate >= base.SuccessRate+0.05
	tokenOK := base.AvgTokenCost == 0 || candidate.AvgTokenCost <= base.AvgTokenCost*1.1
	return successLead && tokenOK
}

// RecordSample 将单次结果合并进适应度统计（EWMA α=0.1）。
func (te *TopologyEvolver) RecordSample(sample *TopologyFitness) {
	if te.fitnessMap == nil {
		te.fitnessMap = make(map[string]*TopologyFitness)
	}
	key := sample.Topology + "|" + sample.TaskType
	existing, ok := te.fitnessMap[key]
	if !ok {
		cp := *sample
		te.fitnessMap[key] = &cp
		return
	}
	alpha := 0.1
	existing.SuccessRate = existing.SuccessRate*(1-alpha) + sample.SuccessRate*alpha
	if sample.AvgTokenCost > 0 {
		existing.AvgTokenCost = existing.AvgTokenCost*(1-alpha) + sample.AvgTokenCost*alpha
	}
	existing.SampleSize++
}

// GetFitness 返回指定拓扑+任务类型的适应度，nil 表示无数据。
func (te *TopologyEvolver) GetFitness(topology, taskType string) *TopologyFitness {
	if te.fitnessMap == nil {
		return nil
	}
	key := topology + "|" + taskType
	f, ok := te.fitnessMap[key]
	if !ok {
		return nil
	}
	cp := *f
	return &cp
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
