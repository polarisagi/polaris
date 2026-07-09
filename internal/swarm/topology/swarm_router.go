package topology

import (
	"context"
	"math/rand/v2"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// SwarmRouter 多 Agent 任务路由器（R7 拆分自 swarm.go）。
// 能力注册表 CapabilityRegistry 见 swarm.go。
// ============================================================================

// SwarmRouter 多 Agent 任务路由器。
//
// 数量限制策略（来自 AgentLimits）：
//   - Registry 上限：注册表总容量，由构造时注入到 CapabilityRegistry
//   - Hierarchy/Pipeline 上限：单任务参与 Agent 数（默认 3，Tier 0 内存约束）
//   - Mesh 上限：单任务参与 Agent 数，同时是拓扑自动升级阈值（默认 10）
//
// 自动拓扑切换：Agent 数量越过 Limits.Mesh 时升级，低于阈值时降回。
// 对应路线图 4.5 多 Agent 拓扑，与 HE-Rule-5（状态机持有控制流）对齐。
type SwarmRouter struct {
	Enabled     bool
	CurrentMode TopologyType
	Limits      AgentLimits // 各维度数量上限，默认 DefaultAgentLimits
	mu          sync.Mutex  // 保护 CurrentMode 并发写安全
	registry    *CapabilityRegistry
	publisher   BlackboardPublisher // hierarchy/pipeline 模式降级路径
}

// NewSwarmRouter 构造路由器。registry 和 publisher 由 M8 Orchestrator 注入。
// 自动将 DefaultAgentLimits().Registry 设置到 registry，调用方无需手动配置上限。
func NewSwarmRouter(enabled bool, registry *CapabilityRegistry, publisher BlackboardPublisher) *SwarmRouter {
	limits := DefaultAgentLimits()
	if registry != nil {
		registry.SetMaxCapacity(limits.Registry)
	}
	return &SwarmRouter{
		Enabled:     enabled,
		CurrentMode: TopologyHierarchy,
		Limits:      limits,
		registry:    registry,
		publisher:   publisher,
	}
}

// RouteTask 根据当前拓扑策略路由任务。
//
// Hierarchy/Pipeline 模式：将任务意图投递到 Blackboard，由 CAS 原子认领机制分配。
// Mesh 模式：查询能力注册表，返回匹配的候选 Agent 列表，Agent 自主拉取（Stigmergy）。
//
// 自动拓扑切换：每次路由前根据注册 Agent 数量动态计算有效模式：
//   - 注册 Agent ≥ meshThreshold(10) → 升级至 Mesh
//   - 注册 Agent < meshThreshold 且当前为 Mesh → 降回 Hierarchy
//
// capabilities 为空时，Mesh 模式返回所有已注册 Agent（由调用方进一步过滤）。
func (s *SwarmRouter) RouteTask(ctx context.Context, intent string, capabilities []Capability) (*RouteResult, error) {
	if !s.Enabled {
		return &RouteResult{Mode: TopologyHierarchy}, nil
	}

	// 自动拓扑切换：根据实时 Agent 注册数动态调整，无需外部 SetMode 调用
	if s.registry != nil {
		count := s.registry.AgentCount()
		s.mu.Lock()
		if count >= s.Limits.Mesh {
			s.CurrentMode = TopologyMesh
		} else if s.CurrentMode == TopologyMesh {
			// Agent 数降到阈值以下时自动回退，避免空 Mesh 路由
			s.CurrentMode = TopologyHierarchy
		}
		mode := s.CurrentMode
		s.mu.Unlock()

		switch mode {
		case TopologyMesh:
			return s.routeMesh(ctx, capabilities)
		default:
			return s.routeHierarchy(ctx, intent)
		}
	}

	s.mu.Lock()
	mode := s.CurrentMode
	s.mu.Unlock()

	switch mode {
	case TopologyMesh:
		return s.routeMesh(ctx, capabilities)
	default:
		return s.routeHierarchy(ctx, intent)
	}
}

// routeHierarchy 通过 Blackboard CAS 投递任务（标准路径）。
func (s *SwarmRouter) routeHierarchy(ctx context.Context, intent string) (*RouteResult, error) {
	if s.publisher == nil {
		// publisher 未注入时降级：返回空路由，由调用方处理
		return &RouteResult{Mode: TopologyHierarchy}, nil
	}
	taskID, err := s.publisher.Publish(ctx, []byte(intent), 0)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SwarmRouter.routeHierarchy", err)
	}
	return &RouteResult{Mode: TopologyHierarchy, TaskID: taskID}, nil
}

// routeMesh 通过能力注册表隐式协调（Stigmergy 模式）。
// 路由成功后自动调用 AcquireLease 增加主选 Agent 负载，形成真实反馈闭环。
// 调用方任务完成时须调用 registry.ReleaseLease(agentID) 归还 lease 计数。
func (s *SwarmRouter) routeMesh(_ context.Context, capabilities []Capability) (*RouteResult, error) {
	if s.registry == nil {
		return &RouteResult{Mode: TopologyMesh}, nil
	}

	agentIDs := s.registry.FindAgents(capabilities)
	if len(agentIDs) == 0 {
		// 无匹配 Agent → 回退 Hierarchy（降级保证任务不丢失）
		return &RouteResult{Mode: TopologyHierarchy}, nil
	}

	// 负载均衡：同等负载下引入随机性，防止热点 Agent
	// FindAgents 已按负载排序，取前 3 名中随机选 1 名作为首选
	topN := min(3, len(agentIDs))
	primary := agentIDs[rand.IntN(topN)]
	// 将首选 Agent 置于结果首位
	ordered := make([]string, 0, len(agentIDs))
	ordered = append(ordered, primary)
	for _, id := range agentIDs {
		if id != primary {
			ordered = append(ordered, id)
		}
	}

	// 路由截断：单任务参与 Agent 数不超过 Limits.Mesh
	// 防止过多 Agent 参与同一任务导致共识噪音（arxiv 2605.03310）
	if s.Limits.Mesh > 0 && len(ordered) > s.Limits.Mesh {
		ordered = ordered[:s.Limits.Mesh]
	}

	// 路由成功：立即递增主选 Agent 负载，使后续 FindAgents 的排序反映真实压力
	s.registry.AcquireLease(primary)

	return &RouteResult{Mode: TopologyMesh, AgentIDs: ordered}, nil
}

// SetMode 手动切换拓扑模式（强制覆盖自动切换逻辑，优先级低于下次 RouteTask 的自动判断）。
// 通常无需调用——RouteTask 已根据 Agent 数量自动切换。
func (s *SwarmRouter) SetMode(mode TopologyType) {
	s.mu.Lock()
	s.CurrentMode = mode
	s.mu.Unlock()
}

// Registry 返回能力注册表（供 M8 Orchestrator 查询）。
func (s *SwarmRouter) Registry() *CapabilityRegistry { return s.registry }
