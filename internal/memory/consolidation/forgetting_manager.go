package consolidation

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// DeleteOldMemories 根据 Ebbinghaus 遗忘曲线清理旧记忆（GD-14-005）
func (fm *ForgettingManager) DeleteOldMemories(ctx context.Context, db protocol.SQLQuerier) error {
	// 升级为 Ebbinghaus 关联度公式：relevance = (accessed_count + 1.0) / (days_since_last_access + 1.0)
	// 保留条件：relevance > 0.1 OR created_at > now - 7days
	query := `
		DELETE FROM episodic_events
		WHERE NOT (
			(accessed_count + 1.0) / (julianday('now') - julianday(last_accessed_at) + 1.0) >= 0.1
			OR created_at > datetime('now', '-7 days')
		)
	`
	_, err := db.ExecContext(ctx, query)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to delete old memories", err)
	}
	return nil
}
