package adapter

import (
	"sort"
	"sync"
)

// ControlVector 已注册的激活引导向量（Activation Steering，M09-Self-Improvement-
// Engine.md §1.3）。Vector 需与目标模型 hidden_size 对齐，由外部离线流程产出
// （如对比激活提取 / contrastive activation addition）——本仓库当前没有任何
// 训练/提取该向量的流水线（区别于 QLoRA/PRM，那是完全不同的技术路线，产出的
// 是模型权重/奖励模型而非 hidden_state 偏移向量），因此本 Store 只负责运行时
// 注册/查询/删除，向量内容需通过 `/steer import <label> <file>` 从外部 JSON
// 文件导入。
type ControlVector struct {
	Label  string
	Vector []float32
	Layer  int // 默认 15（M09 §1.3："同层的多个 Control Vector...默认 layer_id=15"）
}

// ControlVectorStore 线程安全的 label → ControlVector 注册表。
// 进程内存储，不做跨重启持久化：是否需要持久化取决于产品侧运营方式（每次
// 重启后由运维重新 import，还是需要长期保留），属独立于本次"命令面接线"的
// 产品决策，故先满足单进程生命周期内的注册/查询/删除/列举需求（R1：不为
// 尚未提出的持久化需求预先设计存储 schema）。
type ControlVectorStore struct {
	mu    sync.RWMutex
	items map[string]*ControlVector
}

// NewControlVectorStore 创建空注册表。
func NewControlVectorStore() *ControlVectorStore {
	return &ControlVectorStore{items: make(map[string]*ControlVector)}
}

// Import 注册/覆盖一个 label 对应的控制向量。layer<=0 时使用默认层 15。
func (s *ControlVectorStore) Import(label string, vector []float32, layer int) {
	if layer <= 0 {
		layer = 15
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[label] = &ControlVector{Label: label, Vector: vector, Layer: layer}
}

// Get 按 label 查询控制向量。
func (s *ControlVectorStore) Get(label string) (*ControlVector, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cv, ok := s.items[label]
	return cv, ok
}

// Delete 删除指定 label；返回是否确实存在过。
func (s *ControlVectorStore) Delete(label string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[label]; !ok {
		return false
	}
	delete(s.items, label)
	return true
}

// List 返回全部已注册 label（按字典序，便于 /steer list 输出稳定）。
func (s *ControlVectorStore) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.items))
	for k := range s.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
