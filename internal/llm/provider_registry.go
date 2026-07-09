package llm

import (
	"context"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// providerEntry 封装单个 Provider 的运行时状态。
type providerEntry struct {
	provider    protocol.Provider
	name        string
	role        string // general | default | reasoning
	displayName string // 用于 WebUI 展示的友好名称
	cb          *circuitBreaker
	mu          sync.RWMutex
	p95ms       float64 // P95 延迟（指数移动平均）
	successRate float64 // 成功率（指数移动平均，初始 1.0）

}

func newProviderEntry(name, displayName string, p protocol.Provider, cfg config.M1RouterThresholds) *providerEntry {
	return &providerEntry{
		name:        name,
		displayName: displayName,
		provider:    p,
		cb:          newCircuitBreaker(cfg),
		p95ms:       200,
		successRate: 1.0,
	}
}

// healthScore 综合健康评分 = 可用性×0.4 + 延迟×0.3 + 成本×0.2 + 质量×0.1
// 延迟得分 = max(0, 1 - p95ms/5000)；成本得分 = max(0, 1 - costPer1KInput/10)
func (e *providerEntry) healthScore() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	caps := e.provider.Capabilities()
	latencyScore := max64(0, 1.0-e.p95ms/5000.0)
	costScore := max64(0, 1.0-caps.CostPer1KInput/10.0)
	return e.successRate*0.4 + latencyScore*0.3 + costScore*0.2 + 0.1
}

func (e *providerEntry) recordLatency(ms float64) {
	e.mu.Lock()

	e.p95ms = e.p95ms*0.9 + ms*0.1
	e.mu.Unlock()
}

func (e *providerEntry) recordOutcome(success bool, onRecovery func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if success {
		e.successRate = e.successRate*0.95 + 0.05
		if recovered := e.cb.RecordSuccess(); recovered && onRecovery != nil {
			concurrent.SafeGo(context.Background(), "llm.provider_registry.on_recovery", func(context.Context) {
				onRecovery()
			})
		}
	} else {
		e.successRate = e.successRate * 0.95
		e.cb.RecordFailure()
	}
}

// ProviderRegistry 注册/注销 Provider，支持热更新。
type ProviderRegistry struct {
	mu         sync.RWMutex
	entries    map[string]*providerEntry
	onRecovery func(providerName string) // 可选：Provider 熔断恢复时的回调
	cfg        config.M1RouterThresholds // 熔断器配置（来自 M1RouterThresholds TOML）
}

func NewProviderRegistry(cfg config.M1RouterThresholds) *ProviderRegistry {
	return &ProviderRegistry{
		entries: make(map[string]*providerEntry),
		cfg:     cfg,
	}
}

// InjectRecoveryHandler 注入 Provider 恢复回调，由上层（如 InferenceRouter）在初始化时调用。
// fn 在 circuitBreaker HalfOpen→Closed 时触发，providerName 为恢复的 Provider 名称。
func (r *ProviderRegistry) InjectRecoveryHandler(fn func(providerName string)) {
	r.mu.Lock()
	r.onRecovery = fn
	r.mu.Unlock()
}

func (r *ProviderRegistry) Register(name, displayName string, p protocol.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[name] = newProviderEntry(name, displayName, p, r.cfg)
}

func (r *ProviderRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, name)
}

// Get 按注册名返回 Provider（未找到时 ok=false）。
// 用途: 热切换场景需要拿到具体 Provider 实例做类型断言（如
// protocol.LocalProvider.LoadModel），BestForRole/PickProvider 只做路由选优，
// 不满足"按名精确取用"的需求，故补充此按名查找入口。
func (r *ProviderRegistry) Get(name string) (protocol.Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	if !ok {
		return nil, false
	}
	return e.provider, true
}

// UnregisterAll 清空所有注册项，用于热重载前的清理。
func (r *ProviderRegistry) UnregisterAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = make(map[string]*providerEntry)
}

// RegisterWithRole 注册带角色标记的 Provider（general | default | reasoning）。
func (r *ProviderRegistry) RegisterWithRole(name, displayName, role string, p protocol.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := newProviderEntry(name, displayName, p, r.cfg)
	e.role = role
	r.entries[name] = e
}

// BestForRole 返回指定角色下 healthScore 最高的可用 entry。
// 若 role 为空或无匹配则回退到全局 best()。
func (r *ProviderRegistry) BestForRole(role string, req *types.InferRequest) *providerEntry {
	if role == "" || role == "general" {
		return r.best(req)
	}

	chosen := r.findBestByRole(role)
	if chosen == nil {
		return r.best(req)
	}
	return chosen
}

func (r *ProviderRegistry) findBestByRole(role string) *providerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var chosen *providerEntry
	bestScore := -1.0
	for _, e := range r.entries {
		if !e.cb.Allow() {
			continue
		}
		if e.role != role && e.role != "general" {
			continue
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	return chosen
}

// PickProvider 返回指定角色 healthScore 最优的 Provider，供外部直接发起推理。
// 若无可用 Provider 返回 nil。
func (r *ProviderRegistry) PickProvider(role string) protocol.Provider {
	e := r.BestForRole(role, nil)
	if e == nil {
		return nil
	}
	return e.provider
}

// PickProviderByRecordID 尝试通过 model 记录的 UUID 前缀寻找对应的 Provider。
func (r *ProviderRegistry) PickProviderByRecordID(mID string) protocol.Provider {
	if mID == "" {
		return nil
	}
	suffix := mID
	if len(suffix) >= 8 {
		suffix = suffix[:8]
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	var chosen *providerEntry
	bestScore := -1.0
	for name, e := range r.entries {
		if !strings.HasSuffix(name, "/"+suffix) {
			continue
		}
		if !e.cb.Allow() {
			continue
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	if chosen != nil {
		return chosen.provider
	}
	return nil
}

// PickProviderName 返回指定角色最优 Provider 的注册名（含模型标识），供状态展示。
func (r *ProviderRegistry) PickProviderName(role string) string {
	e := r.BestForRole(role, nil)
	if e == nil {
		return ""
	}
	if e.displayName != "" {
		return e.displayName
	}
	return e.name
}

// best 按 healthScore 降序返回第一个 CircuitBreaker 允许且满足多模态能力要求的 entry。
func (r *ProviderRegistry) best(req *types.InferRequest) *providerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	needsVision := req != nil && req.HasImageParts()
	needsVideo := req != nil && req.HasVideoParts()

	var chosen *providerEntry
	bestScore := -1.0
	for _, e := range r.entries {
		if !e.cb.Allow() {
			continue
		}
		caps := e.provider.Capabilities()
		if needsVision && !caps.SupportsVision {
			continue
		}
		if needsVideo && !caps.SupportsVideo {
			continue
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	return chosen
}
