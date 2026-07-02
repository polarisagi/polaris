package agents

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type MemoryWhisper = protocol.MemoryWhisper
type LLMInferFunc = protocol.LLMInferFunc

type OutboxWriterInterface interface {
	Write(ctx context.Context, entry protocol.OutboxEntry) error
}

// MemoryAgent 常驻 goroutine：周期扫描高显著性情景事件生成耳语提示，并驱动记忆图谱边权重维护。
// 统一经 protocol.MemoryFacade 访问记忆子系统，禁止直接 import internal/memory/graph 或裸 SQL
// 查询 episodic_events（M04 §B2 跨模块通信通道）。
type MemoryAgent struct {
	mem          protocol.MemoryFacade
	whisperChan  chan<- MemoryWhisper
	memPressure  *atomic.Int32
	scanInterval time.Duration
	lastSeenID   int64 // 高水位标记：只推送新增事件，防止同批事件每轮重复刷爆耳语通道
}

func NewMemoryAgent(mem protocol.MemoryFacade, whisperChan chan<- MemoryWhisper, memPressure *atomic.Int32) *MemoryAgent {
	return &MemoryAgent{
		mem:          mem,
		whisperChan:  whisperChan,
		memPressure:  memPressure,
		scanInterval: 60 * time.Second,
	}
}

func (ma *MemoryAgent) Run(ctx context.Context) {
	ticker := time.NewTicker(ma.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ma.memPressure != nil && ma.memPressure.Load() >= 2 {
				continue
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
	}
}

func (ma *MemoryAgent) scanHighSalienceEvents(ctx context.Context) error {
	if ma.mem == nil {
		return nil
	}
	scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// id > lastSeenID 高水位过滤：每个事件最多推送一次。
	events, err := ma.mem.ScanHighSalienceEvents(scanCtx, ma.lastSeenID, 0.7, 20)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MemoryAgent.scan", err)
	}

	for _, e := range events {
		if e.ID > ma.lastSeenID {
			ma.lastSeenID = e.ID
		}
		if ma.whisperChan == nil {
			continue
		}
		select {
		case ma.whisperChan <- MemoryWhisper{
			Source:   "memory_agent",
			Salience: e.Salience,
			Content:  fmt.Sprintf("[ID:%d] %s", e.ID, e.Content),
		}:
		default:
			// 通道满：丢弃（耳语是尽力而为的辅助信号，不阻塞主流程）
		}
	}
	return nil
}
