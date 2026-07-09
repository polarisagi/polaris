package optimizer

// PromptOptimizer — GEPA + MemAPO + ContraPrompt 三融合。
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §1.1
// 输出安全流水线（写入 ZoneMutableSkill 前）由 M11 负责，本模块只生产候选。

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// PromptOptimizer 执行 GEPA + MemAPO + ContraPrompt 三融合优化周期。
// 所有依赖通过构造器注入，无全局状态（R1.3）。
type PromptOptimizer struct {
	provider        protocol.Provider   // 高质量模型，用于文本梯度和对比分析（R1.11）
	versionStore    *PromptVersionStore // prompt_versions 表读写层（HE-Rule-6）
	heuristicsStore *HeuristicsStore    // heuristics_memory + fallacy_records 跨会话持久化
	gradientGen     *TextualGradientGenerator
	contrastAna     *ContrastiveAnalyzer
	geneticSearch   *GeneticPromptSearch
	promptMem       *PromptMemory
	errorMem        *ErrorPatternMemory
	maxBudget       int // 软上限 30K tokens/周期
}

// NewPromptOptimizer 构造 PromptOptimizer，provider 和 versionStore 必须非 nil。
func NewPromptOptimizer(provider protocol.Provider, versionStore *PromptVersionStore, maxBudget int) *PromptOptimizer {
	if maxBudget <= 0 {
		maxBudget = 1000000
	}
	return &PromptOptimizer{
		provider:      provider,
		versionStore:  versionStore,
		gradientGen:   &TextualGradientGenerator{provider: provider},
		contrastAna:   &ContrastiveAnalyzer{provider: provider},
		geneticSearch: &GeneticPromptSearch{populationSize: 8, generations: 5},
		promptMem:     &PromptMemory{entries: make(map[string][]*PromptStrategy)},
		errorMem:      &ErrorPatternMemory{patterns: make(map[string]*ErrorPattern)},
		maxBudget:     maxBudget,
	}
}

// NewPromptOptimizerWithDB 构造带 SQL 持久化的 PromptOptimizer。
// db 非 nil 时激活跨会话 heuristics/fallacy 持久化路径；
// 调用方在构造后应显式调用 RestoreOptimizerFromDB 恢复历史状态。
func NewPromptOptimizerWithDB(provider protocol.Provider, versionStore *PromptVersionStore, db protocol.SQLQuerier, maxBudget int) *PromptOptimizer {
	po := NewPromptOptimizer(provider, versionStore, maxBudget)
	if db != nil {
		po.heuristicsStore = NewHeuristicsStore(db)
	}
	return po
}

const maxErrorPatterns = 200

// AddAvoidRule 将错误规避规则注入 ErrorPatternMemory，并异步持久化到 fallacy_records。
// 由 learning.Engine 内环在收到 HeuristicGeneratedEvent 后调用。
func (po *PromptOptimizer) AddAvoidRule(taskType, rule string) {
	if po.errorMem == nil || rule == "" {
		return
	}
	id := fmt.Sprintf("ep_%s_%d", taskType, time.Now().UnixNano())
	pat := &ErrorPattern{
		ID:        id,
		TaskType:  taskType,
		AvoidRule: rule,
		Frequency: 1,
	}
	po.errorMem.mu.Lock()
	if len(po.errorMem.patterns) >= maxErrorPatterns {
		for k := range po.errorMem.patterns {
			delete(po.errorMem.patterns, k)
			break
		}
	}
	po.errorMem.patterns[id] = pat
	po.errorMem.mu.Unlock()
	if po.heuristicsStore != nil {
		p := pat
		concurrent.SafeGo(context.Background(), "prompt_optimizer.save_fallacy", func(_ context.Context) {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = po.heuristicsStore.SaveFallacy(saveCtx, p)
		})
	}
}

// RecordHeuristic 记录成功策略到 PromptMemory，并异步持久化到 heuristics_memory。
// 由 Eval 引擎在版本评分超越基准后调用。
func (po *PromptOptimizer) RecordHeuristic(ctx context.Context, taskType string, s *PromptStrategy) {
	if po.promptMem == nil || s == nil {
		return
	}
	if s.ID == "" {
		s.ID = fmt.Sprintf("hs_%s_%d", taskType, time.Now().UnixNano())
	}
	po.promptMem.mu.Lock()
	po.promptMem.entries[taskType] = append(po.promptMem.entries[taskType], s)
	po.promptMem.mu.Unlock()
	if po.heuristicsStore != nil {
		concurrent.SafeGo(context.Background(), "prompt_optimizer.save_heuristic", func(_ context.Context) {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = po.heuristicsStore.SaveHeuristic(saveCtx, taskType, s)
		})
	}
}

// RestoreOptimizerFromDB 从 DB 恢复跨会话 heuristics 和 fallacy 规则到内存。
// 应在构造后、首次 Optimize 前调用；加载失败不阻断（返回错误供调用方记录）。
func (po *PromptOptimizer) RestoreOptimizerFromDB(ctx context.Context) error {
	if po.heuristicsStore == nil {
		return nil
	}
	heuristics, err := po.heuristicsStore.ListHeuristics(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "prompt_optimizer: load heuristics", err)
	}
	po.promptMem.mu.Lock()
	for taskType, strategies := range heuristics {
		po.promptMem.entries[taskType] = append(po.promptMem.entries[taskType], strategies...)
	}
	po.promptMem.mu.Unlock()

	fallacies, err := po.heuristicsStore.ListFallacies(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "prompt_optimizer: load fallacies", err)
	}
	po.errorMem.mu.Lock()
	maps.Copy(po.errorMem.patterns, fallacies)
	po.errorMem.mu.Unlock()
	return nil
}

// PromptStrategy prompt 策略。
type PromptStrategy struct {
	ID          string
	Template    string
	TriggerCond string
	Source      string
	SuccessRate float64
	UseCount    int
}

// PromptVersion 版本化 prompt。ID 对应 prompt_versions.id（UUID）。
type PromptVersion struct {
	ID        string
	Version   int
	TaskType  string
	Prompt    string
	Score     float64
	Cost      float64
	Source    string
	ParentVer int
	Active    bool
}

// PromptMemory 跨任务 prompt 记忆（MemAPO 用）。
// entries 由 learning.Engine 内环（AddAvoidRule）和 Eval 引擎（RecordHeuristic）
// 跨 goroutine 并发写入，mu 保护读写（R1.3 结构体字段而非全局变量）。
type PromptMemory struct {
	mu      sync.RWMutex
	entries map[string][]*PromptStrategy
}

// ErrorPatternMemory 错误模式记忆（ContraPrompt 用）。
// patterns 同 PromptMemory.entries，存在跨 goroutine 并发写风险，mu 保护读写。
type ErrorPatternMemory struct {
	mu       sync.RWMutex
	patterns map[string]*ErrorPattern
}

// ErrorPattern 错误模式。
type ErrorPattern struct {
	ID           string
	TaskType     string // 任务类型，用于 DB 持久化和按 taskType 过滤
	Description  string
	AvoidRule    string
	Frequency    int
	LinkedMemfID string
}

// TextualGradientGenerator 文本梯度生成器。
// 失败轨迹 → LLM 分析"哪里出错" → 生成优化方向（R1.11：走 provider.Infer）。
type TextualGradientGenerator struct {
	provider protocol.Provider
}

// ContrastiveAnalyzer 对比轨迹分析器。
// 成功 vs 失败轨迹对比 → 提取关键差异（R1.11：走 provider.Infer）。
type ContrastiveAnalyzer struct {
	provider protocol.Provider
}

// GeneticPromptSearch 遗传-Pareto 搜索。
// 种群 8 × 5 代 Pareto 前沿搜索；早停: 连续 2 代前沿无新非支配解。
type GeneticPromptSearch struct {
	populationSize int // 8
	generations    int // 5
	paretoFront    []*PromptVersion
}

// GetParetoFront 返回当前搜索到的 Pareto 前沿。
func (gps *GeneticPromptSearch) GetParetoFront() []*PromptVersion {
	return gps.paretoFront
}

// Optimize 执行 prompt 优化周期，持久化候选到 prompt_versions 表。
//
// 触发条件 (OR):
//  1. tasks ≤ 100 且每 20 次 (冷启动加速)
//  2. score < baseline × 0.95
//  3. tasksSinceLastOpt ≥ 50
//
// 产出经 [Taint-Prop] Gate → Ed25519 签名 → M5 ZoneMutableSkill（由调用方负责）。
func (po *PromptOptimizer) Optimize(ctx context.Context, taskType string, recent []*PromptVersion) []*PromptVersion { //nolint:gocyclo
	if len(recent) == 0 {
		return nil
	}

	// 步骤 1 — MemAPO：从 PromptMemory 检索历史高分策略
	var candidates []*PromptVersion
	if po.promptMem != nil {
		for _, start := range po.promptMem.GetTopStrategies(taskType, 5) {
			candidates = append(candidates, &PromptVersion{
				TaskType: taskType,
				Prompt:   start.Template,
				Score:    start.SuccessRate,
				Source:   "mem_apo",
			})
		}
	}
	// 冷启动：从 DB 恢复历史版本（HE-Rule-6）
	if po.versionStore != nil {
		if hist, err := po.versionStore.ListRecent(ctx, taskType, 5); err == nil {
			candidates = append(candidates, hist...)
		}
	}
	candidates = append(candidates, recent...)

	// 步骤 2 — ContraPrompt：提取 AvoidRules 注入候选 prompt
	var avoidRules []string
	if po.errorMem != nil {
		avoidRules = po.errorMem.GetAvoidRules(taskType)
	}
	if po.contrastAna != nil && len(recent) >= 2 {
		best, worst := findBestWorst(recent)
		diff := po.contrastAna.Analyze(ctx, best.Prompt, worst.Prompt)
		if diff != "" {
			avoidRules = append(avoidRules, "Avoid pattern: "+diff)
		}
	}
	if len(avoidRules) > 0 {
		suffix := "\n[AVOID]: " + joinStrings(avoidRules, "; ")
		for _, c := range candidates {
			c.Prompt += suffix
		}
	}

	// 步骤 3 — GEPA 文本梯度注入
	if po.gradientGen != nil && len(recent) >= 2 {
		best, worst := findBestWorst(recent)
		gradient := po.gradientGen.Generate(ctx, worst.Prompt, best.Prompt)
		if gradient != "" {
			candidates = append(candidates, &PromptVersion{
				TaskType:  taskType,
				Prompt:    gradient,
				Score:     0,
				Source:    "gepa_gradient",
				ParentVer: best.Version,
			})
		}
	}

	// 步骤 4 — GeneticPromptSearch：Pareto 前沿搜索
	if po.geneticSearch != nil {
		candidates = po.geneticSearch.Search(candidates)
	} else {
		candidates = sortByScore(candidates)
	}

	// 步骤 5 — 预算门控：截断候选数量
	maxCandidates := po.budgetLimit()
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}

	// 步骤 6 — 持久化候选到 DB（is_active=0，等候 Eval 评分后激活）
	if po.versionStore != nil {
		po.saveCandiates(ctx, taskType, candidates)
	}

	return candidates
}

// budgetLimit 将 token 预算转换为候选数量上限。
func (po *PromptOptimizer) budgetLimit() int {
	if po.maxBudget <= 0 {
		return 30
	}
	limit := po.maxBudget / 3000
	if limit < 1 {
		return 1
	}
	if limit > 30 {
		return 30
	}
	return limit
}

// saveCandiates 将候选版本写入 prompt_versions 表，忽略单条失败不阻断整体。
func (po *PromptOptimizer) saveCandiates(ctx context.Context, taskType string, candidates []*PromptVersion) {
	for i, c := range candidates {
		if c.ID == "" {
			c.ID = fmt.Sprintf("pv_%s_%d_%d", taskType, time.Now().UnixNano(), i)
		}
		if c.Version == 0 {
			c.Version = int(time.Now().Unix())
		}
		_ = po.versionStore.Save(ctx, c) // 单条失败不阻断，错误已内部记录
	}
}
