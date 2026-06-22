package curriculum

import (
	"github.com/polarisagi/polaris/internal/learning/synthetic"

	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// GapFillWorker 监听 m9_capability_gap Outbox 事件，执行能力补全。
// 架构文档: docs/arch/M09-Self-Improvement.md
type GapFillWorker struct {
	db       protocol.SQLQuerier
	provider protocol.Provider
	registry protocol.ToolRegistry
}

func NewGapFillWorker(db protocol.SQLQuerier, provider protocol.Provider, registry protocol.ToolRegistry) *GapFillWorker {
	return &GapFillWorker{
		db:       db,
		provider: provider,
		registry: registry,
	}
}

// HandleOutbox 实现了 store.OutboxHandler，消费 m9_capability_gap 事件。
func (w *GapFillWorker) HandleOutbox(ctx context.Context, record *store.OutboxRecord) error {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to parse gap payload", err)
	}

	missingTool := w.extractMissingTool(payload.Error)
	if missingTool == "unknown" || missingTool == "" {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("cannot extract tool name from error: %s", payload.Error))
	}

	// 1. 初始化 gap log 记录
	gapID := uuid.New().String()
	_, _ = w.db.ExecContext(ctx, `
		INSERT INTO capability_gap_log (id, session_id, task_id, required_tool, description, status, trust_tier, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, gapID, "unknown", "unknown", missingTool, "Triggered via Outbox", "synthesizing", 1, time.Now().UnixMilli(), time.Now().UnixMilli())

	// 2. 本地合成
	err := w.synthesizeSkill(ctx, missingTool)

	status := "resolved"
	if err != nil {
		status = "failed"
	}

	// 3. 更新状态
	_, _ = w.db.ExecContext(ctx, `
		UPDATE capability_gap_log SET status = ?, updated_at = ? WHERE id = ?
	`, status, time.Now().UnixMilli(), gapID)

	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GapFillWorker.HandleOutbox", err)
	}
	return nil
}

func (w *GapFillWorker) extractMissingTool(errStr string) string {
	// e.g. "tool not found: xyz" or "tool \"xyz\" not found"
	re := regexp.MustCompile(`tool (?:not found: |")?([^"\s:]+)"?`)
	matches := re.FindStringSubmatch(errStr)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return "unknown"
}

func (w *GapFillWorker) synthesizeSkill(ctx context.Context, toolName string) error {
	gen := synthetic.NewSyntheticSkillGen(w.provider, nil)
	skill, err := gen.Generate(ctx, toolName, "Auto-synthesized skill for "+toolName)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GapFillWorker.synthesizeSkill", err)
	}
	if w.registry != nil {
		_ = w.registry.Register(skill)
	}
	return nil
}
