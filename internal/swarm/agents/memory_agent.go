package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

type MemoryWhisper = protocol.MemoryWhisper
type LLMInferFunc = protocol.LLMInferFunc

// MemoryAgent 常驻 goroutine：周期扫描高显著性情景事件生成耳语提示，并驱动记忆图谱边权重维护。
// 统一经 protocol.MemoryFacade 访问记忆子系统，禁止直接 import internal/memory/graph 或裸 SQL
// 查询 episodic_events（docs/specs/04-Module-Boundary.md §B2 跨模块通信通道）。
type MemoryAgent struct {
	mem          protocol.MemoryFacade
	whisperChan  chan<- MemoryWhisper
	memPressure  *atomic.Int32
	admitter     BackgroundAdmitter
	scanInterval time.Duration
	lastSeenID   int64 // 高水位标记：只推送新增事件，防止同批事件每轮重复刷爆耳语通道
	schedulers   []SyncScheduler
	cursors      WhisperCursorStore
	cursorLoaded bool
}

// whisperCursorID 是耳语高水位在统一游标表 consumer_cursors 中的 consumer_id。
const whisperCursorID = "memory_agent.whisper"

// WhisperCursorStore 耳语高水位的持久化端口（消费端接口，nil 安全）。
// 无持久化时高水位仅在内存，重启后会把全部高显著事件重推一遍耳语通道。
type WhisperCursorStore interface {
	GetCursor(ctx context.Context, consumerID string) (int64, error)
	SaveCursor(ctx context.Context, consumerID string, seq int64) error
}

type SyncScheduler interface {
	Start(ctx context.Context) error
}

// BackgroundAdmitter 是 MemoryAgent 周期维护使用的资源准入端口。
// @producer: internal/automation.ResourceGovernor
type BackgroundAdmitter interface {
	AdmitBackground(work string) (release func(), admitted bool)
}

func NewMemoryAgent(mem protocol.MemoryFacade, whisperChan chan<- MemoryWhisper, memPressure *atomic.Int32) *MemoryAgent {
	return NewMemoryAgentWithAdmitter(mem, whisperChan, memPressure, nil)
}

// NewMemoryAgentWithAdmitter 将周期维护接入宿主已有的后台资源准入器。
func NewMemoryAgentWithAdmitter(mem protocol.MemoryFacade, whisperChan chan<- MemoryWhisper, memPressure *atomic.Int32, admitter BackgroundAdmitter) *MemoryAgent {
	return &MemoryAgent{
		mem:          mem,
		whisperChan:  whisperChan,
		memPressure:  memPressure,
		admitter:     admitter,
		scanInterval: 60 * time.Second,
	}
}

// SetCursorStore 注入高水位持久化；须在 Run 之前调用。
func (ma *MemoryAgent) SetCursorStore(cs WhisperCursorStore) {
	ma.cursors = cs
}

// loadCursor 首次扫描前从持久化恢复高水位（仅一次）。读失败告警后从 0 起，
// 代价是重推旧耳语，好过启动失败。
func (ma *MemoryAgent) loadCursor(ctx context.Context) {
	if ma.cursorLoaded || ma.cursors == nil {
		return
	}
	ma.cursorLoaded = true
	seq, err := ma.cursors.GetCursor(ctx, whisperCursorID)
	if err != nil {
		slog.Warn("memory_agent: 读取耳语高水位失败，从 0 开始", "err", err)
		return
	}
	if seq > ma.lastSeenID {
		ma.lastSeenID = seq
	}
}

func (ma *MemoryAgent) RegisterSyncScheduler(s SyncScheduler) {
	ma.schedulers = append(ma.schedulers, s)
}

func (ma *MemoryAgent) Run(ctx context.Context) {
	for _, s := range ma.schedulers {
		sched := s
		concurrent.SafeGo(ctx, "memory_agent.sync_scheduler", func(ctx context.Context) {
			if err := sched.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("memory_agent: sync scheduler failed", "err", err)
			}
		})
	}

	ticker := time.NewTicker(ma.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ma.runMaintenanceCycle(ctx)
		}
	}
}

func (ma *MemoryAgent) runMaintenanceCycle(ctx context.Context) {
	if ma.memPressure != nil && ma.memPressure.Load() >= 2 {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	if ma.admitter != nil {
		release, admitted := ma.admitter.AdmitBackground("memory_agent_maintenance")
		if !admitted {
			return
		}
		if release != nil {
			defer release()
		}
	}
	if err := ma.scanHighSalienceEvents(ctx); err != nil {
		slog.Error("memory_agent: scan failed", "err", err)
	}
	if ma.mem != nil {
		if err := ma.mem.PruneMemoryGraph(ctx); err != nil {
			slog.Error("memory_agent: prune failed", "err", err)
		}
	}
}

func (ma *MemoryAgent) scanHighSalienceEvents(ctx context.Context) error {
	if ma.mem == nil {
		return nil
	}
	scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ma.loadCursor(ctx)
	// id > lastSeenID 高水位过滤：每个事件最多推送一次。
	events, err := ma.mem.ScanHighSalienceEvents(scanCtx, ma.lastSeenID, 0.7, 20)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MemoryAgent.scan", err)
	}

	for _, e := range events {
		if e.ID > ma.lastSeenID {
			ma.lastSeenID = e.ID
		}
		if ma.whisperChan == nil || !e.MetadataValid || e.ProjectID != types.DefaultProjectID ||
			e.TaintLevel < types.TaintNone || e.TaintLevel > types.TaintUserReviewed {
			continue
		}
		select {
		case ma.whisperChan <- MemoryWhisper{
			Source:        protocol.MemoryWhisperSourceMemoryAgent,
			Salience:      e.Salience,
			Content:       fmt.Sprintf("[ID:%d] %s", e.ID, e.Content),
			ProjectID:     e.ProjectID,
			TaintLevel:    e.TaintLevel,
			MetadataValid: true,
		}:
		default:
			// 通道满：丢弃（耳语是尽力而为的辅助信号，不阻塞主流程）
		}
	}
	// 每批推送后持久化；写失败仅告警，下次重启最多重推本批。
	if len(events) > 0 && ma.cursors != nil {
		if err := ma.cursors.SaveCursor(ctx, whisperCursorID, ma.lastSeenID); err != nil {
			slog.Warn("memory_agent: 持久化耳语高水位失败", "err", err)
		}
	}
	return nil
}
