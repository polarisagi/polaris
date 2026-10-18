package sleeptime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/learning/harnessoverlay"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ConsolidationRunner 会话记忆整理函数契约。
type ConsolidationRunner func(ctx context.Context, sessionID string) error

// Coordinator 协调 sleep-time 会话整理与 harness 自改进闭环。
type Coordinator struct {
	overlayMgr      harnessoverlay.HarnessOverlayManager
	evaluator       Evaluator
	proposer        LLMProposer
	db              protocol.SQLQuerier
	auditTrail      AuditTrail
	offPeak         offPeakGate
	consolidate     ConsolidationRunner
	enabled         bool
	idleMinutes     int
	maxFailuresScan int
}

// CoordinatorConfig 配置参数。
type CoordinatorConfig struct {
	OverlayMgr      harnessoverlay.HarnessOverlayManager
	Evaluator       Evaluator
	Proposer        LLMProposer
	DB              protocol.SQLQuerier
	AuditTrail      AuditTrail
	OffPeak         offPeakGate
	Consolidate     ConsolidationRunner
	Enabled         bool
	IdleMinutes     int
	MaxFailuresScan int
}

// NewCoordinator 创建 Coordinator 实例。
func NewCoordinator(cfg CoordinatorConfig) *Coordinator {
	idle := cfg.IdleMinutes
	if idle <= 0 {
		idle = 10
	}
	scan := cfg.MaxFailuresScan
	if scan <= 0 {
		scan = 100
	}
	return &Coordinator{
		overlayMgr:      cfg.OverlayMgr,
		evaluator:       cfg.Evaluator,
		proposer:        cfg.Proposer,
		db:              cfg.DB,
		auditTrail:      cfg.AuditTrail,
		offPeak:         cfg.OffPeak,
		consolidate:     cfg.Consolidate,
		enabled:         cfg.Enabled,
		idleMinutes:     idle,
		maxFailuresScan: scan,
	}
}

// SetEnabled 设置闭环启用状态（由信任档位策略或配置决定）。
func (c *Coordinator) SetEnabled(enabled bool) {
	c.enabled = enabled
}

// RunClosedLoop 执行一次 Self-Harness 自改进闭环（ADR-0115 WP6）。
// 流程：1. 挖掘失败模式 -> 2. 提出 1~3 改动 -> 3. 双切分验证 -> 4. 裁决合并或拒绝 -> 5. 记录审计事件。
func (c *Coordinator) RunClosedLoop(ctx context.Context) (*LoopResult, error) {
	if c.overlayMgr == nil || c.evaluator == nil || c.proposer == nil {
		return nil, apperr.New(apperr.CodeInternal, "sleeptime.RunClosedLoop: missing required dependencies")
	}

	// 1. 挖掘近期失败模式
	weaknesses, err := MineFailures(ctx, c.db, c.maxFailuresScan)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime: mine failures", err)
	}
	if len(weaknesses) == 0 {
		slog.DebugContext(ctx, "sleeptime: no failures found for self-improvement")
		return &LoopResult{Accepted: false, RejectionReason: "no weaknesses detected"}, nil
	}

	// 2. 提出 1~3 个最小改动
	proposals, err := c.proposer.Propose(ctx, weaknesses)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime: propose changes", err)
	}
	if len(proposals) == 0 {
		return &LoopResult{Weaknesses: weaknesses, Accepted: false, RejectionReason: "no valid proposals generated"}, nil
	}

	// 3. 在 held-in / held-out 双切分回归集上验证
	accepted, baseIn, baseOut, candIn, candOut, reason, err := VerifyProposals(ctx, c.evaluator, proposals)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime: verify proposals", err)
	}

	result := &LoopResult{
		Weaknesses:       weaknesses,
		Proposals:        proposals,
		BaselineHeldIn:   baseIn,
		BaselineHeldOut:  baseOut,
		CandidateHeldIn:  candIn,
		CandidateHeldOut: candOut,
		Accepted:         accepted,
		RejectionReason:  reason,
	}

	// 4. 裁决分支
	if !accepted {
		slog.InfoContext(ctx, "sleeptime: harness proposals rejected by eval", "reason", reason)
		if c.auditTrail != nil {
			_ = c.auditTrail.Record(ctx, "polaris", "harness_overlay_rejected", "eval", map[string]any{
				"reason":            reason,
				"baseline_held_in":  baseIn,
				"baseline_held_out": baseOut,
				"candidate_held_in": candIn,
				"candidate_held_out": candOut,
				"proposals_count":   len(proposals),
			})
		}
		return result, nil
	}

	// 5. 接受：写入覆盖层并提交 git 版本
	files := make(map[string][]byte, len(proposals))
	for _, p := range proposals {
		files[p.Path] = []byte(p.Content)
	}
	commitMsg := fmt.Sprintf("feat(overlay): auto-improve harness (held-in %.2f->%.2f, held-out %.2f->%.2f)",
		baseIn, candIn, baseOut, candOut)

	hash, err := c.overlayMgr.WriteAndCommit(ctx, files, commitMsg, "Polaris Self-Improvement <polaris@local>")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime: write and commit overlay", err)
	}
	result.CommitHash = hash

	slog.InfoContext(ctx, "sleeptime: harness proposals accepted and committed", "hash", hash, "reason", reason)
	if c.auditTrail != nil {
		_ = c.auditTrail.Record(ctx, "polaris", "harness_overlay_commit", hash, map[string]any{
			"hash":              hash,
			"reason":            reason,
			"baseline_held_in":  baseIn,
			"baseline_held_out": baseOut,
			"candidate_held_in": candIn,
			"candidate_held_out": candOut,
			"proposals_count":   len(proposals),
		})
	}
	return result, nil
}

// FindIdleSessions 查询空闲超过 idleMinutes 的会话列表。
func (c *Coordinator) FindIdleSessions(ctx context.Context, idleMinutes int) ([]string, error) {
	if c.db == nil {
		return nil, nil
	}
	if idleMinutes <= 0 {
		idleMinutes = 10
	}

	query := `
		SELECT id FROM chat_sessions
		WHERE updated_at < strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-' || ? || ' minutes')
		ORDER BY updated_at DESC
		LIMIT 50
	`
	rows, err := c.db.QueryContext(ctx, query, idleMinutes)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime.FindIdleSessions: query", err)
	}
	defer rows.Close()

	var sessions []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && id != "" {
			sessions = append(sessions, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime.FindIdleSessions: rows", err)
	}
	return sessions, nil
}

// RunSleepTime 执行空闲期任务：错峰检查、会话整理与自改进闭环。
func (c *Coordinator) RunSleepTime(ctx context.Context) error {
	if !c.enabled {
		return nil
	}
	if c.offPeak != nil && !c.offPeak.Allow() {
		slog.DebugContext(ctx, "sleeptime: skipping, outside off-peak window")
		return nil
	}

	// 1. 查找空闲会话并触发记忆整理
	idleSessions, err := c.FindIdleSessions(ctx, c.idleMinutes)
	if err != nil {
		slog.WarnContext(ctx, "sleeptime: find idle sessions failed", "err", err)
	} else if len(idleSessions) > 0 && c.consolidate != nil {
		for _, sID := range idleSessions {
			if err := c.consolidate(ctx, sID); err != nil {
				slog.WarnContext(ctx, "sleeptime: session consolidation failed", "session_id", sID, "err", err)
			}
		}
	}

	// 2. 触发一次失败挖掘与自改进闭环
	if _, err := c.RunClosedLoop(ctx); err != nil {
		slog.WarnContext(ctx, "sleeptime: closed loop execution failed", "err", err)
		return err
	}
	return nil
}
