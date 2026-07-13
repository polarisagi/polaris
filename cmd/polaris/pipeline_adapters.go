package main

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/types"
)

// ─── diagLoggerAdapter ────────────────────────────────────────────────────────

type diagLoggerAdapter struct{}

func (d *diagLoggerAdapter) AppendDecision(ctx context.Context, entry *types.DecisionLogEntry) error {
	slog.InfoContext(ctx, "pipeline event", "session_id", entry.SessionID, "agent_id", entry.AgentID, "decision", entry.DecisionType, "choice", entry.Choice, "reason", entry.Reason)
	return nil
}
