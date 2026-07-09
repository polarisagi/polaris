package topology

import (
	"context"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// TopologyType 多 Agent 协作拓扑类型。
type TopologyType string

const (
	TopologyHierarchy TopologyType = "hierarchy" // 树状层级（默认，走 Blackboard CAS）
	TopologyPipeline  TopologyType = "pipeline"  // 线性流水线
	TopologyMesh      TopologyType = "mesh"      // 动态网状（Stigmergy，Tier 1+）
)

// meshThreshold 触发自动升级到 Mesh 拓扑的 Agent 数量阈值。
// 研究数据：3-4 Agent 团队是 Sequential Pipeline 最优区间；10+ Agent 才需要 Stigmergy 隐式协调。
const meshThreshold = 10

// AgentLimits 各维度的 Agent 数量上限。
//
// 研究依据（arxiv 2605.03310）：
//   - Sequential Pipeline（3-4 Agent）Brier 0.153，排名第一
//   - Consensus Alignment（多 Agent 共识）Brier 0.181，排名最差
//   - 超过 10 Agent：共识机制引入噪音，正确意见被多数错误否决
//   - Tier 0 内存预算：Go goroutine 并发上限 3（docs/research_archive/00-research-archive.md §3）
//
// 质量原则：一个有独特角色的高质量 Agent > 多个角色重叠的普通 Agent。
type AgentLimits struct {
	Registry  int // 注册表全局容量上限（0 = 不限）；默认 10
	Hierarchy int // Hierarchy/Pipeline 模式单任务最大参与 Agent 数；默认 3（Tier 0 内存约束）
	Pipeline  int // Pipeline 模式线性阶段数上限；默认 5
	Mesh      int // Mesh 模式单任务最大参与 Agent 数，同时是拓扑自动升级阈值；默认 10
}

// DefaultAgentLimits 返回默认限制（Tier 0 基准，可在 NewSwarmRouter 后覆盖）。
func DefaultAgentLimits() AgentLimits {
	return AgentLimits{
		Registry:  10,            // 整个系统最多注册 10 个 Agent
		Hierarchy: 3,             // Tier 0 内存预算：3 goroutines
		Pipeline:  5,             // 流水线最多 5 个阶段
		Mesh:      meshThreshold, // 与自动升级阈值对齐
	}
}

var (
	// ErrRegistryFull 注册表已达容量上限，新 Agent 无法加入。
	ErrRegistryFull = apperr.New(apperr.CodeInternal, "swarm: agent registry at capacity")
	// ErrDuplicateCapabilities 已存在能力集合完全相同的 Agent，禁止角色冗余。
	ErrDuplicateCapabilities = apperr.New(apperr.CodeInternal, "swarm: identical capability set already registered by another agent")
)

// Capability Agent 能力标签（字符串枚举，由 Agent 注册时自声明）。
type Capability string

// AgentCapabilities 单个 Agent 的能力声明与运行时负载。
type AgentCapabilities struct {
	AgentID      string
	Capabilities []Capability
	Load         int // 当前活跃 lease 数量，用于负载均衡排序
}

// RouteResult RouteTask 的结构化路由结果。
type RouteResult struct {
	Mode     TopologyType
	TaskID   string   // Hierarchy/Pipeline 模式：Blackboard 任务 ID
	AgentIDs []string // Mesh 模式：匹配能力的候选 Agent ID（已按负载排序）
}

// BlackboardPublisher 是对 Blackboard.Submit 的最小接口抽象。
// 由上层（M8 Orchestrator）注入，避免 topology 直接依赖 swarm 父包的具体类型。
type BlackboardPublisher interface {
	// Publish 将任务意图投递到 Blackboard，返回分配的任务 ID。
	Publish(ctx context.Context, intent []byte, priority int) (taskID string, err error)
}

// CapabilityRegistry 能力注册表（内存，进程生命周期）。
// Agent 启动时 Register，下线时 Unregister。
// 内置容量上限与角色唯一性门控：拒绝超额注册与重复能力集，强制每个 Agent 持有独特角色。
type CapabilityRegistry struct {
	mu          sync.RWMutex
	byAgent     map[string]*AgentCapabilities // agentID → capabilities
	byCap       map[Capability][]string       // capability → []agentID（发布-订阅索引）
	maxCapacity int                           // 0 = 不限；由 SetMaxCapacity 或 SwarmRouter 初始化时设置
}

// NewCapabilityRegistry 创建空注册表（无容量上限）。
// 生产环境推荐通过 SwarmRouter 统一管理限制——NewSwarmRouter 会自动调用 SetMaxCapacity。
func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{
		byAgent: make(map[string]*AgentCapabilities),
		byCap:   make(map[Capability][]string),
	}
}

// SetMaxCapacity 设置注册表容量上限（0 = 不限）。线程安全，可在运行时调整。
func (r *CapabilityRegistry) SetMaxCapacity(n int) {
	r.mu.Lock()
	r.maxCapacity = n
	r.mu.Unlock()
}

// Register 注册 Agent 的能力集合。
//
// 返回 ErrRegistryFull：注册表已满且该 Agent 未曾注册。
// 返回 ErrDuplicateCapabilities：已有其他 Agent 持有完全相同的能力集合（角色冗余）。
// 重复注册同一 agentID 以最新为准，不触发上述限制。
func (r *CapabilityRegistry) Register(caps AgentCapabilities) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, isRenew := r.byAgent[caps.AgentID]

	if !isRenew {
		// 容量门控：全局上限检查
		if r.maxCapacity > 0 && len(r.byAgent) >= r.maxCapacity {
			return ErrRegistryFull
		}
		// 角色唯一性门控：拒绝能力集合完全相同的新 Agent
		if r.hasIdenticalCapabilitiesLocked(caps.AgentID, caps.Capabilities) {
			return ErrDuplicateCapabilities
		}
	}

	// 清理旧注册（renew 路径）
	if isRenew {
		for _, c := range r.byAgent[caps.AgentID].Capabilities {
			r.removeByCap(c, caps.AgentID)
		}
	}

	entry := caps
	r.byAgent[caps.AgentID] = &entry
	for _, c := range caps.Capabilities {
		r.byCap[c] = append(r.byCap[c], caps.AgentID)
	}
	return nil
}

// hasIdenticalCapabilitiesLocked 检查是否已有其他 Agent 持有完全相同的能力集合。
// 调用方须持有写锁。
func (r *CapabilityRegistry) hasIdenticalCapabilitiesLocked(selfID string, caps []Capability) bool {
	if len(caps) == 0 {
		return false
	}
	incoming := make(map[Capability]struct{}, len(caps))
	for _, c := range caps {
		incoming[c] = struct{}{}
	}
	for id, agent := range r.byAgent {
		if id == selfID || len(agent.Capabilities) != len(caps) {
			continue
		}
		match := true
		for _, c := range agent.Capabilities {
			if _, ok := incoming[c]; !ok {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Unregister 注销 Agent。
func (r *CapabilityRegistry) Unregister(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.byAgent[agentID]
	if !ok {
		return
	}
	for _, c := range old.Capabilities {
		r.removeByCap(c, agentID)
	}
	delete(r.byAgent, agentID)
}

// UpdateLoad 更新 Agent 当前负载（由心跳或 Blackboard Lease 计数调用）。
func (r *CapabilityRegistry) UpdateLoad(agentID string, load int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.byAgent[agentID]; ok {
		a.Load = load
	}
}

// AcquireLease 任务路由成功后递增 Agent 活跃 lease 计数。
// 由 routeMesh 在选定主 Agent 后自动调用，形成真实负载反馈闭环。
func (r *CapabilityRegistry) AcquireLease(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.byAgent[agentID]; ok {
		a.Load++
	}
}

// ReleaseLease 任务完成/失败后递减 Agent 活跃 lease 计数。
// 由 Blackboard Reaper 或 Agent 完成回调触发，Load 不低于 0。
func (r *CapabilityRegistry) ReleaseLease(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.byAgent[agentID]; ok {
		if a.Load > 0 {
			a.Load--
		}
	}
}

// AgentCount 返回当前注册的 Agent 总数（O(1)，用于拓扑自动切换判断）。
func (r *CapabilityRegistry) AgentCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byAgent)
}

// FindAgents 返回具备所有指定能力的 Agent ID 列表，已按负载升序排序。
// 空 capabilities 参数返回所有已注册 Agent。
func (r *CapabilityRegistry) FindAgents(capabilities []Capability) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(capabilities) == 0 {
		ids := make([]string, 0, len(r.byAgent))
		for id := range r.byAgent {
			ids = append(ids, id)
		}
		return ids
	}

	// 取各能力集合的交集
	candidates := r.intersect(capabilities)
	if len(candidates) == 0 {
		return nil
	}

	// 按负载排序（负载最低的优先）
	type scored struct {
		id   string
		load int
	}
	scored_ := make([]scored, 0, len(candidates))
	for _, id := range candidates {
		if a, ok := r.byAgent[id]; ok {
			scored_ = append(scored_, scored{id: id, load: a.Load})
		}
	}
	// 简单插入排序（候选数量通常 <10）
	for i := 1; i < len(scored_); i++ {
		for j := i; j > 0 && scored_[j].load < scored_[j-1].load; j-- {
			scored_[j], scored_[j-1] = scored_[j-1], scored_[j]
		}
	}

	result := make([]string, len(scored_))
	for i, s := range scored_ {
		result[i] = s.id
	}
	return result
}

// intersect 计算多个能力集合的 agentID 交集（内部方法，调用方持有读锁）。
func (r *CapabilityRegistry) intersect(caps []Capability) []string {
	if len(caps) == 0 {
		return nil
	}
	// 以最小集合为起点，逐步过滤
	base := r.byCap[caps[0]]
	if len(base) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(base))
	for _, id := range base {
		result[id] = struct{}{}
	}
	for _, c := range caps[1:] {
		next := make(map[string]struct{}, len(result))
		for _, id := range r.byCap[c] {
			if _, ok := result[id]; ok {
				next[id] = struct{}{}
			}
		}
		result = next
		if len(result) == 0 {
			return nil
		}
	}
	ids := make([]string, 0, len(result))
	for id := range result {
		ids = append(ids, id)
	}
	return ids
}

func (r *CapabilityRegistry) removeByCap(cap Capability, agentID string) {
	list := r.byCap[cap]
	for i, id := range list {
		if id == agentID {
			r.byCap[cap] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// SwarmRouter 多 Agent 任务路由器见 swarm_router.go（R7 拆分）。
