package connector

import (
	"sync"
)

// Registry 用于管理所有注册的 KnowledgeSourceConnector
type Registry struct {
	mu         sync.RWMutex
	connectors map[string]KnowledgeSourceConnector
}

// NewRegistry 构造一个空的 Connector 注册表。
//
// 2026-07-04 审计修复：此前是包级可变全局变量 GlobalRegistry，直接违反
// CLAUDE.md「internal/ 禁全局可变变量（ADR-0001 豁免仅限 observability/metrics）」
// 强制规则。现在改为由调用方（cmd/polaris 启动流程）显式创建单例并通过依赖注入
// 传递给 MCPInstaller 和知识摄入调度装配代码，不再有包级可变状态。
func NewRegistry() *Registry {
	return &Registry{
		connectors: make(map[string]KnowledgeSourceConnector),
	}
}

// Register 注册一个 Connector
func (r *Registry) Register(c KnowledgeSourceConnector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectors[c.ID()] = c
}

// Unregister 注销一个 Connector
func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.connectors, id)
}

// Get 返回所有已注册的 Connector
func (r *Registry) GetAll() []KnowledgeSourceConnector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make([]KnowledgeSourceConnector, 0, len(r.connectors))
	for _, c := range r.connectors {
		res = append(res, c)
	}
	return res
}
