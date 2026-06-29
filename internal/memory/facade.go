package memory

import "context"

// MemoryFacade 记忆系统对外统一接口。
//
// 问题背景：
//
//	当前 memory 包有两套接口：
//	- memory.MemorySystem（memory.go）：Write/Retrieve/Consolidate/Forget/Mem()
//	- protocol.Memory（protocol/interfaces.go）：Working()/Episodic()/Semantic()/Procedural()
//	新代码不知道该走哪个，导致双轨并存。
//
// 解决方案：
//   - MemoryFacade 是 memory 包对外的统一入口，内部持有 MemorySystem 实现
//   - 上层模块（agent、swarm）通过 MemoryFacade 操作，不直接持有 MemorySystem struct
//   - protocol.Memory 接口继续作为子层（四层记忆各自接口）的契约，不废弃
//
// @consumer: agent/agent.go, swarm/agents/memory_agent.go, gateway/server/server.go
// @producer: memory.DefaultMemorySystem（由 cli.go/bootstrap 构造注入）
type MemoryFacade interface {
	// Write 写入一条记忆条目（自动路由到对应层）。
	Write(ctx context.Context, entry *MemoryEntry) error
	// Retrieve 混合检索（BM25 + 向量 + 图，自动融合）。
	Retrieve(ctx context.Context, query *RetrievalQuery) ([]MemoryEntry, error)
	// Consolidate 触发 L1→L2 记忆巩固（冷数据蒸馏为语义实体）。
	Consolidate(ctx context.Context) error
	// Forget 清理过期/低权重记忆，返回删除条数。
	Forget(ctx context.Context) (int, error)
	// System 返回底层 MemorySystem（供需要细粒度操作的代码使用）。
	// 注：绝大多数调用方只需 Write/Retrieve，直接调用 System() 是例外而非常规。
	System() MemorySystem
}

// ─── 实现 ─────────────────────────────────────────────────────────────────────

// MemoryFacadeImpl 包装 MemorySystem 提供统一门面。
type MemoryFacadeImpl struct {
	sys MemorySystem
}

// NewMemoryFacade 构造记忆门面。
func NewMemoryFacade(sys MemorySystem) *MemoryFacadeImpl {
	return &MemoryFacadeImpl{sys: sys}
}

func (f *MemoryFacadeImpl) Write(ctx context.Context, entry *MemoryEntry) error {
	return f.sys.Write(ctx, entry)
}

func (f *MemoryFacadeImpl) Retrieve(ctx context.Context, query *RetrievalQuery) ([]MemoryEntry, error) {
	return f.sys.Retrieve(ctx, query)
}

func (f *MemoryFacadeImpl) Consolidate(ctx context.Context) error {
	return f.sys.Consolidate(ctx)
}

func (f *MemoryFacadeImpl) Forget(ctx context.Context) (int, error) {
	return f.sys.Forget(ctx)
}

func (f *MemoryFacadeImpl) System() MemorySystem {
	return f.sys
}
