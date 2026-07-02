package memory

import (
	"context"
	"time"

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
	entities, err := f.sys.Mem().Semantic().SearchEntities(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	// 污点门控：过滤高于调用方允许等级的实体（ADR-0007 只升不降，读侧按上限截断）
	filtered := entities[:0]
	for _, e := range entities {
		if int(e.TaintLevel) <= maxTaint {
			filtered = append(filtered, e)
		}
	}
	return filtered, nil
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

// ArchiveEpisodic 将会话的历史事件标记为冷数据（滑动窗口边界 = 当前时刻）。
// 语义对齐 ConsolidationPipeline.MarkColdEpisodicEvents（M05 §3.2 Session Compaction）。
func (f *MemoryFacadeImpl) ArchiveEpisodic(ctx context.Context, sessionID string) error {
	_, err := f.sys.Mem().Episodic().MarkCold(ctx, sessionID, time.Now())
	return err
}

// AddWorkingContext 向 L0 工作记忆上下文窗口追加一条文本（热路径，不持久化）。
func (f *MemoryFacadeImpl) AddWorkingContext(_ context.Context, text string) error {
	wm := f.sys.Mem().Working()
	if wm == nil || wm.Context() == nil {
		return nil
	}
	wm.Context().Append(types.Message{Role: "assistant", Content: text})
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
