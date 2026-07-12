package orchestrator

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// AgentRegistry 负责 Agent 的注册与能力发现。
type AgentRegistry struct {
	mu     sync.RWMutex
	agents map[string]*AgentHandle
}

// NewAgentRegistry 创建一个新的注册表。
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{
		agents: make(map[string]*AgentHandle),
	}
}

// Register 注册一个新 Agent。
// 如果相同 ID 的 Agent 已存在，将先注销旧实例再注册新实例。
func (r *AgentRegistry) Register(id string, card AgentCard, handle any) error {
	if id == "" {
		return apperr.New(apperr.CodeInvalidInput, "agent ID cannot be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.agents[id] = &AgentHandle{
		Card:         card,
		Handle:       handle,
		RegisteredAt: time.Now().Unix(),
		Status:       "active",
	}

	return nil
}

// Deregister 注销 Agent。
func (r *AgentRegistry) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if handle, ok := r.agents[id]; ok {
		handle.Status = "inactive"
		delete(r.agents, id)
	}
}

// MarkUnreachable 将心跳超时或断开连接的 Agent 标记为不可达，使其不参与调度匹配。
func (r *AgentRegistry) MarkUnreachable(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if handle, ok := r.agents[id]; ok {
		handle.Status = "unreachable"
	}
}

// Get 获取指定的 AgentHandle。
func (r *AgentRegistry) Get(id string) (*AgentHandle, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	handle, ok := r.agents[id]
	return handle, ok
}

// FindBestAgent 根据所需能力寻找最适合处理任务的 Agent。
// Phase 1: 硬过滤 (DeclaresCapabilities >= RequiredCapabilities)
// Phase 2: 加权降序 (Laplace 成功率 + 负载率)
func (r *AgentRegistry) FindBestAgent(requiredCapabilities []string, currentLoads map[string]int, attemptStats map[string]AgentStats) (*AgentHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type candidate struct {
		id     string
		handle *AgentHandle
		score  float64
	}

	var candidates []candidate //nolint:prealloc

	for id, handle := range r.agents {
		if handle.Status != "active" {
			continue
		}

		// Phase 1: 硬过滤
		if !containsAll(handle.Card.Skills, requiredCapabilities) {
			continue
		}

		// Phase 2: 打分
		stats := attemptStats[id]
		successCount := float64(stats.SuccessCount)
		attemptCount := float64(stats.AttemptCount)

		// Laplace 平滑
		prior := 0.5
		priorStrength := 2.0
		// 通用 prior=0.5/str=2, 专精型若技能数较少可赋予更高先验(如0.3/str=6)，此处统一按通用算
		laplaceSuccRate := (successCount + prior*priorStrength) / (attemptCount + priorStrength)

		load := float64(currentLoads[id])
		if load < 0 {
			load = 0
		}
		loadFactor := 1.0 / math.Max(load, 1.0)

		// score = 0.6 * LaplaceSuccRate + 0.4 * LoadFactor
		score := 0.6*laplaceSuccRate + 0.4*loadFactor

		candidates = append(candidates, candidate{
			id:     id,
			handle: handle,
			score:  score,
		})
	}

	if len(candidates) == 0 {
		return nil, apperr.New(apperr.CodeNotFound, "no suitable agent found")
	}

	// 降序排列
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	return candidates[0].handle, nil
}

// AgentStats 用于记录 Agent 的成功和尝试次数。
type AgentStats struct {
	SuccessCount int
	AttemptCount int
}

func containsAll(declared, required []string) bool {
	if len(required) == 0 {
		return true
	}
	declMap := make(map[string]bool)
	for _, req := range declared {
		declMap[req] = true
	}
	for _, req := range required {
		if !declMap[req] {
			return false
		}
	}
	return true
}
