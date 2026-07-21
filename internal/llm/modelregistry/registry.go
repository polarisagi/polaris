// Package modelregistry 实现 P3-2 ModelVersionRegistry：模型版本/废弃状态/
// 兼容性评分管理，驱动三档自动迁移策略与 Embedding 模型废弃时的重嵌联动。
// 架构文档: docs/arch/M01-Inference-Runtime.md §9
package modelregistry

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// consecutiveErrorRollbackThreshold 连续 4xx/5xx 失败达到此阈值时，
// RecordCallResult 建议调用方自动回退到旧模型。
const consecutiveErrorRollbackThreshold = 3

// MigrationDecision 三档自动迁移策略结果。
type MigrationDecision int

const (
	// MigrationAuto CompatibilityScore >= 0.9：允许自动切换到继任模型，无需人工确认。
	MigrationAuto MigrationDecision = iota
	// MigrationAutoWithWarn 0.7 <= score < 0.9：允许自动切换，但记录 WARN 供运营关注。
	MigrationAutoWithWarn
	// MigrationManualOnly score < 0.7：禁止自动切换，必须人工确认后手动迁移。
	MigrationManualOnly
)

func (d MigrationDecision) String() string {
	switch d {
	case MigrationAuto:
		return "auto"
	case MigrationAutoWithWarn:
		return "auto_with_warn"
	case MigrationManualOnly:
		return "manual_only"
	default:
		return "unknown"
	}
}

// DecideMigration 按 CompatibilityScore 返回三档迁移策略。
// 阈值来源: docs/arch/M01-Inference-Runtime.md §9（>=0.9 自动 / 0.7-0.9 自动+WARN / <0.7 禁止自动）。
func DecideMigration(compatibilityScore float64) MigrationDecision {
	switch {
	case compatibilityScore >= 0.9:
		return MigrationAuto
	case compatibilityScore >= 0.7:
		return MigrationAutoWithWarn
	default:
		return MigrationManualOnly
	}
}

// SkillCompatTester 对指定 Provider/Model 重跑某个技能的兼容性测试。
// @consumer: Registry.OnModelUpgrade
// @producer: 未来由 internal/eval/harness 或 internal/extension/skill 提供具体实现；
//
//	当前无生产实现时，调用方可传 nil，OnModelUpgrade 会跳过重测、仅更新元数据。
type SkillCompatTester interface {
	TestSkillCompat(ctx context.Context, provider, modelID, skillName string) (passed bool, err error)
}

// ReindexTrigger 在 Embedding 模型被标记废弃时触发（Embedding 模型全量重嵌，
// 见 docs/arch/M02-Storage-Fabric.md OnlineReindexer）。
//
// 设计说明：M2 OnlineReindexer（internal/memory/retrieval/online_reindexer.go）
// 本身是版本差异驱动的（比较 episodic_events.embed_model_version 与当前
// Embedder.ModelVersion()），只要新 Embedder 生效后下一次 Run() 就会自然
// 重嵌所有旧版本行——不需要一次"精确重嵌某个模型"的命令式触发。此回调仅用于
// "尽快"语义（例如主动唤醒后台重嵌循环提前跑一轮，而非等待默认 5min 周期），
// 不传（nil）时不影响正确性，只影响重嵌开始的及时性。
type ReindexTrigger func(ctx context.Context, provider, modelID string) error

// Registry 是 ModelVersionRegistry 的业务逻辑层，包装 repo.ModelVersionRepository。
type Registry struct {
	repo           repo.ModelVersionRepository
	reindexTrigger ReindexTrigger
}

// Option 配置 Registry 的可选行为。
type Option func(*Registry)

// WithReindexTrigger 注入 Embedding 模型废弃时的重嵌唤醒回调。
func WithReindexTrigger(fn ReindexTrigger) Option {
	return func(r *Registry) { r.reindexTrigger = fn }
}

// NewRegistry 构造 ModelVersionRegistry。repo 必须非 nil。
func NewRegistry(store repo.ModelVersionRepository, opts ...Option) *Registry {
	r := &Registry{repo: store}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func entryID(provider, modelID string) string {
	return provider + ":" + modelID
}

// Get 按 provider+modelID 查询条目，未注册时返回 (nil, nil)。
func (r *Registry) Get(ctx context.Context, provider, modelID string) (*repo.ModelVersionEntry, error) {
	return r.repo.Get(ctx, entryID(provider, modelID))
}

// List 返回全部已注册条目。
func (r *Registry) List(ctx context.Context) ([]*repo.ModelVersionEntry, error) {
	return r.repo.List(ctx)
}

// getOrNew 取现有条目，不存在则构造一个默认值合理的新条目（不落盘，由调用方 Upsert）。
func (r *Registry) getOrNew(ctx context.Context, provider, modelID string) (*repo.ModelVersionEntry, error) {
	id := entryID(provider, modelID)
	entry, err := r.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		entry = &repo.ModelVersionEntry{
			ID:                 id,
			Provider:           provider,
			ModelID:            modelID,
			CompatibilityScore: 1.0, // 新模型默认视为兼容，OnModelUpgrade 重测后覆盖
			ValidatedOn:        "[]",
			Capabilities:       "{}",
		}
	}
	return entry, nil
}

// OnModelUpgrade 处理模型版本升级：对每个受影响技能重跑兼容性测试，更新
// ValidatedOn 与 CompatibilityScore；score < 0.8 时记录 WARN（docs/arch §9）。
// tester 为 nil 时跳过实测，仅刷新 UpdatedAt（供尚无兼容测试实现的早期阶段调用）。
func (r *Registry) OnModelUpgrade(ctx context.Context, provider, modelID string, skillNames []string, tester SkillCompatTester) error {
	entry, err := r.getOrNew(ctx, provider, modelID)
	if err != nil {
		return err
	}

	if tester != nil && len(skillNames) > 0 {
		passed := 0
		validated := make([]string, 0, len(skillNames))
		for _, name := range skillNames {
			ok, testErr := tester.TestSkillCompat(ctx, provider, modelID, name)
			if testErr != nil {
				slog.Warn("polaris: model compat test failed to run, skipping skill",
					"provider", provider, "model", modelID, "skill", name, "err", testErr)
				continue
			}
			if ok {
				passed++
				validated = append(validated, name)
			}
		}
		entry.CompatibilityScore = float64(passed) / float64(len(skillNames))
		validatedJSON, jsonErr := json.Marshal(validated)
		if jsonErr != nil {
			return apperr.Wrap(apperr.CodeInternal, "modelregistry: marshal ValidatedOn", jsonErr)
		}
		entry.ValidatedOn = string(validatedJSON)
	}

	entry.UpdatedAt = time.Now().Unix()
	// DecideMigration 2026-07-21 deadcode 审查补齐：此前只有一道 ad-hoc 的
	// score<0.8 单档 WARN，三档迁移策略（M01 §9：>=0.9 自动 / 0.7-0.9 自动+WARN /
	// <0.7 禁止自动）从未被真正用于任何决策，只是文档描述的纯函数从未被调用。
	// 注意：这里只做可观测日志，不执行自动切换——路由层把某个 ProviderRegistry
	// 条目背后的 modelID 热替换为继任模型需要改造条目结构本身（见 router.go
	// recordModelCallResult 同类说明），是更大的设计变更，此处不强行代为决定。
	decision := DecideMigration(entry.CompatibilityScore)
	switch decision {
	case MigrationManualOnly:
		slog.Warn("polaris: model compatibility score requires manual migration decision",
			"provider", provider, "model", modelID, "score", entry.CompatibilityScore,
			"migration_decision", decision.String())
	case MigrationAutoWithWarn:
		slog.Warn("polaris: model compatibility score allows auto-migration with caution",
			"provider", provider, "model", modelID, "score", entry.CompatibilityScore,
			"migration_decision", decision.String())
	case MigrationAuto:
		slog.Info("polaris: model compatibility score allows unattended auto-migration",
			"provider", provider, "model", modelID, "score", entry.CompatibilityScore,
			"migration_decision", decision.String())
	}
	return r.repo.Upsert(ctx, entry)
}

// DeprecateModel 标记模型废弃并设置继任模型；若该模型具备 embedding 能力
// （Capabilities.embedding=true），唤醒 ReindexTrigger（见其文档）。
func (r *Registry) DeprecateModel(ctx context.Context, provider, modelID, successorModelID string) error {
	entry, err := r.getOrNew(ctx, provider, modelID)
	if err != nil {
		return err
	}
	entry.Deprecated = true
	entry.SuccessorModelID = successorModelID
	entry.UpdatedAt = time.Now().Unix()
	if err := r.repo.Upsert(ctx, entry); err != nil {
		return err
	}

	// 记录本次废弃在当前 CompatibilityScore 下的迁移策略档位，供 sysadmin/运维
	// 判断该继任模型切换是否需要人工确认（同 OnModelUpgrade 的 DecideMigration 用法）。
	slog.Info("polaris: model deprecated",
		"provider", provider, "model", modelID, "successor", successorModelID,
		"score", entry.CompatibilityScore, "migration_decision", DecideMigration(entry.CompatibilityScore).String())

	if r.reindexTrigger != nil && hasEmbeddingCapability(entry) {
		if triggerErr := r.reindexTrigger(ctx, provider, modelID); triggerErr != nil {
			slog.Warn("polaris: reindex trigger failed after model deprecation",
				"provider", provider, "model", modelID, "err", triggerErr)
		}
	}
	return nil
}

func hasEmbeddingCapability(e *repo.ModelVersionEntry) bool {
	if e.Capabilities == "" {
		return false
	}
	var caps map[string]any
	if err := json.Unmarshal([]byte(e.Capabilities), &caps); err != nil {
		return false
	}
	v, ok := caps["embedding"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// RecordCallResult 记录一次模型调用的成败，驱动连续失败自动回退（docs/arch §9：
// 连续 3 次 4xx/5xx 自动回退）。success=true 时清零连续失败计数；success=false
// 累加计数，达到阈值时 shouldRollback=true，并尝试通过 FindPredecessor 找到
// "把 successor_model_id 指向当前模型"的旧条目作为回退目标
// （rollbackToModelID 为空表示无已知旧模型可回退，调用方需自行决策降级路径）。
// 对未注册模型（Get 返回 nil）调用是安全的 no-op（不追踪未注册模型的失败率）。
func (r *Registry) RecordCallResult(ctx context.Context, provider, modelID string, success bool) (shouldRollback bool, rollbackToModelID string, err error) {
	id := entryID(provider, modelID)
	entry, err := r.repo.Get(ctx, id)
	if err != nil {
		return false, "", err
	}
	if entry == nil {
		return false, "", nil
	}

	if success {
		if entry.ConsecutiveErrors != 0 {
			entry.ConsecutiveErrors = 0
			entry.UpdatedAt = time.Now().Unix()
			if upsertErr := r.repo.Upsert(ctx, entry); upsertErr != nil {
				return false, "", upsertErr
			}
		}
		return false, "", nil
	}

	entry.ConsecutiveErrors++
	entry.UpdatedAt = time.Now().Unix()
	if upsertErr := r.repo.Upsert(ctx, entry); upsertErr != nil {
		return false, "", upsertErr
	}
	if entry.ConsecutiveErrors < consecutiveErrorRollbackThreshold {
		return false, "", nil
	}

	predecessor, findErr := r.repo.FindPredecessor(ctx, provider, modelID)
	if findErr != nil {
		return true, "", findErr
	}
	if predecessor == nil {
		return true, "", nil
	}
	return true, predecessor.ModelID, nil
}

// ─── resolveXXXModel() 静态映射迁移 ─────────────────────────────────────────

type staticMapping struct {
	provider   string
	deprecated string
	successor  string
}

// staticResolverMappings 镜像各 Adapter resolveXXXModel() 中的硬编码废弃名映射
// （internal/llm/adapter/{anthropic,openai,deepseek}.go）。resolveXXXModel()
// 本身不删除、不改行为（dev prompt 显式要求保留为无数据库依赖的编译期兜底
// 路径）——此表只是把同一份事实数据额外镜像进可查询的 DB registry，供需要
// CompatibilityScore/SuccessorModelID 结构化查询的调用方使用，两者是同一份
// 数据的两种消费方式，不是竞争实现。
var staticResolverMappings = []staticMapping{
	{"anthropic", "claude-instant-1.2", "claude-3-5-haiku-latest"},
	{"anthropic", "claude-2.0", "claude-3-5-haiku-latest"},
	{"anthropic", "claude-2.1", "claude-3-5-haiku-latest"},
	{"anthropic", "claude-3-opus-20240229", "claude-3-5-sonnet-latest"},
	{"openai", "gpt-3.5-turbo", "gpt-4o-mini"},
	{"openai", "gpt-4", "gpt-4o-mini"},
	{"openai", "gpt-4-turbo", "gpt-4o"},
	{"openai", "gpt-4-turbo-preview", "gpt-4o"},
	{"deepseek", "deepseek-chat", "deepseek-v4-flash"},
	{"deepseek", "deepseek-reasoner", "deepseek-v4-pro"},
}

// SeedFromStaticResolvers 将 staticResolverMappings 灌入 registry，幂等
// （已存在的条目不覆盖，避免抹掉运营中产生的真实 CompatibilityScore/
// ConsecutiveErrors 数据）。CompatibilityScore 初始值 1.0——这些映射已在
// 生产环境的 resolveXXXModel() 路径长期运行，视为已验证安全。
func (r *Registry) SeedFromStaticResolvers(ctx context.Context) error {
	for _, m := range staticResolverMappings {
		id := entryID(m.provider, m.deprecated)
		existing, err := r.repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if existing != nil {
			continue
		}
		entry := &repo.ModelVersionEntry{
			ID:                 id,
			Provider:           m.provider,
			ModelID:            m.deprecated,
			Deprecated:         true,
			SuccessorModelID:   m.successor,
			CompatibilityScore: 1.0,
			ValidatedOn:        "[]",
			Capabilities:       "{}",
			UpdatedAt:          time.Now().Unix(),
		}
		if err := r.repo.Upsert(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}
