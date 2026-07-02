package agents

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/memory/graph"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type MemoryWhisper = protocol.MemoryWhisper
type LLMInferFunc = protocol.LLMInferFunc

type OutboxWriterInterface interface {
	Write(ctx context.Context, entry protocol.OutboxEntry) error
}

type MemoryAgent struct {
	db            protocol.SQLQuerier
	whisperChan   chan<- MemoryWhisper
	memPressure   *atomic.Int32
	scanInterval  time.Duration
	edgeWeightMgr *graph.EdgeWeightManager
}

func NewMemoryAgent(db protocol.SQLQuerier, store protocol.Store, whisperChan chan<- MemoryWhisper, memPressure *atomic.Int32) *MemoryAgent {
	return &MemoryAgent{
		db:            db,
		whisperChan:   whisperChan,
		memPressure:   memPressure,
		scanInterval:  60 * time.Second,
		edgeWeightMgr: graph.NewEdgeWeightManager(store),
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
			if err := ma.edgeWeightMgr.PeriodicPrune(ctx); err != nil {
				slog.Error("memory_agent: prune failed", "err", err)
			}
		}
	}
}

func (ma *MemoryAgent) scanHighSalienceEvents(ctx context.Context) error {
	scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := ma.db.QueryContext(scanCtx, `
		SELECT id, session_id, content, salience, occurred_at 
		FROM episodic_events
		WHERE archived = 0 AND salience >= 0.7
		ORDER BY occurred_at DESC LIMIT 20
	`)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MemoryAgent.scan", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var sessionID, content string
		var salience float64
		var occurredAt int64
		if err := rows.Scan(&id, &sessionID, &content, &salience, &occurredAt); err == nil {
			if ma.whisperChan == nil {
				continue
			}
			select {
			case ma.whisperChan <- MemoryWhisper{
				Source:   "memory_agent",
				Salience: salience,
				Content:  fmt.Sprintf("[ID:%d] %s", id, content),
			}:
			default:
				// Channel full, skip
			}
		}
	}
	return nil
}
