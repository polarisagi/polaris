package surprise

import "sync"

// DriftDowngradeRegistry 记录哪些 task_type 当前处于"降级纯 BM25"状态。
// M05 §12.3 降级表: "Embedding DriftDetector 检测到漂移 | 该 task_type 降级
// 纯 BM25，其余不受影响 | Blue-Green 重嵌完成后切回"。
//
// 该注册表是 DriftOrchestrator（写入）与 HybridRetrieverImpl.Search（读取）
// 之间的唯一耦合点：HE-3 要求跨模块走结构化状态而非直接依赖对方内部实现，
// 这里用一个线程安全的 map 承载"降级中的 task_type 集合"这一最小状态。
type DriftDowngradeRegistry struct {
	mu         sync.RWMutex
	downgraded map[string]bool
}

// NewDriftDowngradeRegistry 创建空注册表。
func NewDriftDowngradeRegistry() *DriftDowngradeRegistry {
	return &DriftDowngradeRegistry{downgraded: make(map[string]bool)}
}

// IsDowngraded 查询指定 task_type 当前是否处于降级状态。
// taskType 为空字符串（无法分类）时始终返回 false——不降级未分类查询。
func (r *DriftDowngradeRegistry) IsDowngraded(taskType string) bool {
	if taskType == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.downgraded[taskType]
}

// SetDowngraded 设置/清除指定 task_type 的降级状态。
func (r *DriftDowngradeRegistry) SetDowngraded(taskType string, v bool) {
	if taskType == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if v {
		r.downgraded[taskType] = true
	} else {
		delete(r.downgraded, taskType)
	}
}

// ClearAll 清空全部降级状态（Blue-Green 重嵌完成后整体切回）。
func (r *DriftDowngradeRegistry) ClearAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.downgraded = make(map[string]bool)
}

// Downgraded 返回当前所有处于降级状态的 task_type（用于日志/观测，非热路径调用）。
func (r *DriftDowngradeRegistry) Downgraded() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.downgraded))
	for k := range r.downgraded {
		out = append(out, k)
	}
	return out
}
