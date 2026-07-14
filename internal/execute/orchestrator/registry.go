package orchestrator

import (
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// AgentRegistry 负责 Agent 的注册与能力发现。
// 2026-07-14：FindBestAgent（负载加权评分选主）随中心化 Orchestrator 一并删除
// （生产环境 RegisterWorker 从未调用，评分逻辑零消费者，见 ADR-0050）。
// AgentRegistry 本体保留：SQLiteBlackboard.SetRegistry 用它做 SpawnDepth 校验
// （真实生产依赖），agent-0 也在启动时注册进来。
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

// AgentHandle Agent 句柄。
type AgentHandle struct {
	Card         AgentCard
	Handle       any // 本地 chan 或远程 A2A gRPC
	RegisteredAt int64
	Status       string // active | inactive | unreachable
}

// AgentCard Agent 能力声明（A2A v0.3 兼容）。
type AgentCard struct {
	Name          string
	Version       string
	Description   string
	Skills        []string
	Tools         []string
	Models        []string
	MaxConcurrent int
	TrustLevel    int
	SandboxTier   int
	Endpoint      string
	MaxDepth      int // 0 表示使用全局 MaxSpawnDepth 默认值
}
