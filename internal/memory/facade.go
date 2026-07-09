package memory

import (
	"context"
	"time"

	memgraph "github.com/polarisagi/polaris/internal/memory/graph"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MemoryFacadeImpl 包装 MemorySystem 提供统一门面。
type MemoryFacadeImpl struct {
	sys MemorySystem
	// edgeMgr 图谱边权重维护器，仅 NewMemoryFacadeWithStore 构造时非 nil。
	// nil 时 PruneMemoryGraph 静默跳过（Tier0 无周期维护场景）。
	edgeMgr *memgraph.EdgeWeightManager
}

// 编译期校验
var _ protocol.MemoryFacade = (*MemoryFacadeImpl)(nil)

// NewMemoryFacade 构造记忆门面。
func NewMemoryFacade(sys MemorySystem) *MemoryFacadeImpl {
	return &MemoryFacadeImpl{sys: sys}
}

// NewMemoryFacadeWithStore 构造带图谱周期维护能力的记忆门面。
// 供需要驱动 PruneMemoryGraph 的调用方使用（如 swarm.MemoryAgent 常驻 goroutine），
// 避免该调用方直接 import internal/memory/graph 构造 EdgeWeightManager（M04 §B2）。
func NewMemoryFacadeWithStore(sys MemorySystem, store protocol.Store) *MemoryFacadeImpl {
	f := &MemoryFacadeImpl{sys: sys}
	if store != nil {
		f.edgeMgr = memgraph.NewEdgeWeightManager(store)
	}
	return f
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
	entities, err := f.sys.Mem().Semantic().SearchEntities(ctx, query, topK, 0)
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
func (f *MemoryFacadeImpl) ListEpisodicEvents(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error) {
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

// ListCoreMemory 读取核心工作记忆块（UP-03 ZoneCoreMemory 注入源）。
// Working 层或 CoreMemory 存储未配置时静默返回空，调用方按无核心记忆处理。
func (f *MemoryFacadeImpl) ListCoreMemory(ctx context.Context, agentID, sessionID string) ([]types.CoreMemoryBlock, error) {
	wm := f.sys.Mem().Working()
	if wm == nil || wm.CoreMemory() == nil {
		return nil, nil
	}
	blocks, err := wm.CoreMemory().List(ctx, agentID, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "memory_facade: list core memory", err)
	}
	return blocks, nil
}

// Reflection 层调用
func (f *MemoryFacadeImpl) ListReflections(ctx context.Context, q types.ReflectionQuery) ([]types.ReflectionEntry, error) {
	if rm := f.sys.Mem().Reflection(); rm != nil {
		entries, err := rm.ListReflections(ctx, q)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "list reflections failed", err)
		}
		return entries, nil
	}
	return nil, nil
}

func (f *MemoryFacadeImpl) AppendReflection(ctx context.Context, entry types.ReflectionEntry) error {
	if rm := f.sys.Mem().Reflection(); rm != nil {
		return rm.AppendReflection(ctx, entry)
	}
	return nil
}

// 后台维护调用（swarm.MemoryAgent 等常驻 goroutine 通过本门面驱动，见 protocol.MemoryFacade）
func (f *MemoryFacadeImpl) ScanHighSalienceEvents(ctx context.Context, sinceID int64, minSalience float64, limit int) ([]types.SalienceEvent, error) {
	ep := f.sys.Mem().Episodic()
	if ep == nil {
		return nil, nil
	}
	return ep.ScanHighSalience(ctx, sinceID, minSalience, limit)
}

func (f *MemoryFacadeImpl) PruneMemoryGraph(ctx context.Context) error {
	if f.edgeMgr == nil {
		return nil
	}
	return f.edgeMgr.PeriodicPrune(ctx)
}

// TaskMermaidCanvas 调用（M05 §11.3），委托给底层 MemorySystem 共享单实例。
func (f *MemoryFacadeImpl) TrackToolCall(toolUseID, toolName string) {
	f.sys.Mem().TrackToolCall(toolUseID, toolName)
}

func (f *MemoryFacadeImpl) TrackToolResult(toolUseID string, success bool, summary string) {
	f.sys.Mem().TrackToolResult(toolUseID, success, summary)
}

func (f *MemoryFacadeImpl) RenderTaskCanvas() string {
	return f.sys.Mem().RenderTaskCanvas()
}

// legacy (for memory system internals)
func (f *MemoryFacadeImpl) Write(ctx context.Context, entry *MemoryEntry) error {
	return f.sys.Write(ctx, entry)
}
func (f *MemoryFacadeImpl) List(ctx context.Context, query *RetrievalQuery) ([]MemoryEntry, error) {
	return f.sys.List(ctx, query) //nolint:wrapcheck // 门面透传，语义与同段 Write/Consolidate/Forget 一致
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
