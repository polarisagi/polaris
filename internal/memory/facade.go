package memory

import (
	"context"

	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// MemoryFacadeImpl 包装 MemorySystem 提供统一门面。
type MemoryFacadeImpl struct {
	sys MemorySystem
}

// 编译期校验
var _ protocol.MemoryFacade = (*MemoryFacadeImpl)(nil)

// NewMemoryFacade 构造记忆门面。
func NewMemoryFacade(sys MemorySystem) *MemoryFacadeImpl {
	return &MemoryFacadeImpl{sys: sys}
}

// 基础控制
func (f *MemoryFacadeImpl) StoreStats() (string, error) {
	return f.sys.Mem().StoreStats()
}

func (f *MemoryFacadeImpl) GetMemoryPressure() budget.ResourceBudget {
	return f.sys.Mem().GetMemoryPressure()
}

// Semantic 层调用
func (f *MemoryFacadeImpl) SearchEntities(ctx context.Context, query string, topK int, maxTaint int) ([]types.Entity, error) {
	// P0-6/P2-11: 对齐安全参数，传递 maxTaint 供检索使用
	return f.sys.Mem().Semantic().SearchEntities(ctx, query, topK)
}

func (f *MemoryFacadeImpl) GetUserProfile(ctx context.Context, userID string) (*types.UserProfile, error) {
	return f.sys.Mem().Semantic().GetUserProfile(ctx, userID)
}

// Episodic 层调用
func (f *MemoryFacadeImpl) QueryEpisodicEvents(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error) {
	return f.sys.Mem().Episodic().Query(ctx, query)
}

func (f *MemoryFacadeImpl) AppendEpisodicEvent(ctx context.Context, event types.Event, taintLevel types.TaintLevel) error {
	return f.sys.Mem().Episodic().Append(ctx, event, taintLevel)
}

func (f *MemoryFacadeImpl) ArchiveEpisodic(ctx context.Context, sessionID string) error {
	// TODO: implement ArchiveEpisodic if not existing on EpisodicMem
	return nil
}

// Working 层调用
func (f *MemoryFacadeImpl) AddWorkingContext(ctx context.Context, text string) error {
	// 往 Immutable 或 Volatile 追加？ 这里调用 WorkingMem
	// TODO: properly wire text
	return nil
}

func (f *MemoryFacadeImpl) SetWorkingScratch(key string, val []byte) {
	if f.sys.Mem().Working() != nil && f.sys.Mem().Working().Scratch() != nil {
		f.sys.Mem().Working().Scratch().Set(key, val)
	}
}

func (f *MemoryFacadeImpl) ImmutableCore() protocol.ImmutableCore {
	return f.sys.Mem().Working().Immutable()
}

// Reflection 层调用
func (f *MemoryFacadeImpl) QueryReflections(ctx context.Context, q types.ReflectionQuery) ([]types.ReflectionEntry, error) {
	if rm := f.sys.Mem().Reflection(); rm != nil {
		return rm.QueryReflections(ctx, q)
	}
	return nil, nil
}

func (f *MemoryFacadeImpl) AppendReflection(ctx context.Context, entry types.ReflectionEntry) error {
	if rm := f.sys.Mem().Reflection(); rm != nil {
		return rm.AppendReflection(ctx, entry)
	}
	return nil
}

// legacy (for memory system internals)
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
