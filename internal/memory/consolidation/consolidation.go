package consolidation

import (
	"github.com/polarisagi/polaris/internal/memory/retrieval"

	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// Consolidation — Episodic → Semantic 记忆压缩管线。
// 架构文档: docs/arch/M05-Memory-System.md §4

// ConsolidationPipeline 4 阶段压缩管线。
// 触发: 主题转换 shift → 立即触发 | eventCount ≥ 50 → 触发 | sessionClosed → 强制触发.
//
// 依赖注入:
//   - episodic: 读取待压缩的 Episodic 事件
//   - semantic: 写入提取出的实体/关系/摘要
//   - skills:   Stage 4 Logic Collapse 注册新 Skill（nil 时跳过）
//   - provider: LLM 提取实体/摘要（nil 时走规则 fallback）
type ConsolidationPipeline struct {
	episodic     protocol.EpisodicMemory
	semantic     protocol.SemanticMemory
	skills       protocol.SkillRegistry
	provider     protocol.Provider
	writeFilter  *retrieval.WriteFilter
	cascadeInv   *retrieval.CascadeInvalidator
	db           protocol.SQLQuerier
	gate         backgroundGate
	skillEvolver SkillEvolver
}

type backgroundGate interface {
	BackgroundPermit(priority int) bool
}

// SkillEvolver 定义了技能后台演化的接口（如 Logic Collapse 触发）。
type SkillEvolver interface {
	CheckAndEvolve(ctx context.Context) error
}

func (p *ConsolidationPipeline) WithBackgroundGate(g backgroundGate) { p.gate = g }
func (p *ConsolidationPipeline) WithSkillEvolver(e SkillEvolver)     { p.skillEvolver = e }

// NewConsolidationPipeline 创建压缩管线，episodic 和 semantic 必须非 nil。
func NewConsolidationPipeline(
	episodic protocol.EpisodicMemory,
	semantic protocol.SemanticMemory,
	skills protocol.SkillRegistry,
	provider protocol.Provider,
) *ConsolidationPipeline {
	return &ConsolidationPipeline{
		episodic: episodic,
		semantic: semantic,
		skills:   skills,
		provider: provider,
	}
}

// NewConsolidationPipelineFull 创建完整的固化流水线。
func NewConsolidationPipelineFull(
	e protocol.EpisodicMemory,
	s protocol.SemanticMemory,
	skills protocol.SkillRegistry,
	provider protocol.Provider,
	wf *retrieval.WriteFilter,
	ci *retrieval.CascadeInvalidator,
	db protocol.SQLQuerier,
) *ConsolidationPipeline {
	return &ConsolidationPipeline{
		episodic:    e,
		semantic:    s,
		skills:      skills,
		provider:    provider,
		writeFilter: wf,
		cascadeInv:  ci,
		db:          db,
	}
}

// consolidationTimeout Consolidation 管线最大运行时间（兜底防止阻塞 M9 调度器）
const consolidationTimeout = 5 * time.Minute

// Run 执行完整 4 阶段压缩管线。
// 约束: version++ 不可变版本 + source_event_id provenance + 信念修正 + Prospective Indexing.
// 超时: 整体 5 分钟超时（独立于 ctx 父超时），防止 LLM 调用长时间阻塞调度器。
func (p *ConsolidationPipeline) Run(ctx context.Context, sessionID string) error {
	if p.gate != nil && !p.gate.BackgroundPermit(2) {
		return nil // skip if not permitted
	}
	if p.episodic == nil || p.semantic == nil {
		return apperr.New(apperr.CodeInternal, "consolidation: episodic and semantic memory required")
	}

	// 整体超时保护：Consolidation 为后台任务，不应无限阻塞
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ctx, cancel = context.WithTimeout(ctx, consolidationTimeout)
		defer cancel()
	}

	// 查询该 Session 的所有 Episodic 事件
	events, err := p.episodic.Query(ctx, types.EpisodicQuery{
		SessionID:     sessionID,
		K:             200,
		MaxTaintLevel: types.TaintHigh, // 允许读取带污点的事件，并在巩固时向上传播
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "consolidation: query episodic events", err)
	}
	if len(events) == 0 {
		return nil
	}

	return p.executeStages(ctx, sessionID, events)
}

func (p *ConsolidationPipeline) executeStages(ctx context.Context, sessionID string, events []types.ScoredEvent) error {
	// Stage 1 — 实体/关系提取
	entities, relations, err := p.extractEntitiesAndRelations(ctx, sessionID, events)
	if err != nil {
		// 非阻断：Stage 1 失败不中止后续阶段
		entities = nil
		relations = nil
	}

	// Stage 2 — Upsert Semantic Memory (传播污点)
	maxTaint := computeMaxTaint(events)
	if err := p.upsertSemantic(ctx, entities, relations, maxTaint); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "consolidation: stage2 upsert", err)
	}

	// Stage 3 — 会话摘要生成
	if err := p.summarizeSession(ctx, sessionID, events); err != nil {
		slog.Warn("consolidation: stage3 summarizeSession failed", "err", err)
	}

	// Stage 3.5 — 用户画像合成（L3 Persona）
	// 触发条件: events ≥ 10（保证最低信号量）。异步友好，失败不阻断。
	// 来源: supermemory User Profile + TencentDB L3 Persona 收敛方案。
	if len(events) >= 10 {
		if err := p.synthesizeUserProfile(ctx, events); err != nil {
			slog.Warn("consolidation: stage3.5 synthesizeUserProfile failed", "err", err)
		}
	}

	// Stage 4 — Logic Collapse → Skill Library
	if p.skills != nil {
		if err := p.updateSkills(ctx, sessionID, events); err != nil {
			slog.Warn("consolidation: stage4 updateSkills failed", "err", err)
		}
	}

	// Stage 5 — 技能后台演化监控 (Trigger Logic Collapse Regeneration)
	if p.skillEvolver != nil {
		if err := p.skillEvolver.CheckAndEvolve(ctx); err != nil {
			slog.Warn("consolidation: stage5 CheckAndEvolve failed", "err", err)
		}
	}

	return nil
}

// MarkColdEpisodicEvents 滑动窗口算法：找出 1 小时以前且未被固化的事件，打上 cold 标签。
// 这是短程记忆降维 (C2) 的实现。
func (p *ConsolidationPipeline) MarkColdEpisodicEvents(ctx context.Context, sessionID string) error {
	if p.episodic == nil {
		return nil
	}

	// 1 小时前的事件
	before := time.Now().Add(-1 * time.Hour)
	_, err := p.episodic.MarkCold(ctx, sessionID, before)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "consolidation: failed to mark cold episodic events", err)
	}

	return nil
}

// ─── Stage 1 ─────────────────────────────────────────────────────────────────

// extractEntitiesAndRelations 从 Episodic 事件中提取实体与关系。
// 主路径: LLM 提取（provider 非 nil）。回退: 正则/共现规则。
func computeMaxTaint(events []types.ScoredEvent) types.TaintLevel {
	maxTaint := types.TaintNone
	for _, ev := range events {
		if event, _ := ev.Event.(*types.Event); event != nil {
			if event.TaintLevel > maxTaint {
				maxTaint = event.TaintLevel
			}
		}
	}
	if maxTaint < types.TaintMedium {
		maxTaint = types.TaintMedium
	}
	return maxTaint
}
