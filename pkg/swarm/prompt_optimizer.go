package swarm

// PromptOptimizer — GEPA + MemAPO + ContraPrompt 三融合。
// 架构文档: docs/arch/09-Self-Improvement-Engine-深度选型.md §1.1
// 输出安全流水线（写入 ZoneMutableSkill 前）由 M11 负责，本模块只生产候选。

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// PromptOptimizer 执行 GEPA + MemAPO + ContraPrompt 三融合优化周期。
// 所有依赖通过构造器注入，无全局状态（R1.3）。
type PromptOptimizer struct {
	provider         protocol.Provider   // 高质量模型，用于文本梯度和对比分析（R1.11）
	versionStore     *PromptVersionStore // prompt_versions 表读写层（HE-Rule-6）
	heuristicsStore  *HeuristicsStore    // heuristics_memory + fallacy_records 跨会话持久化
	gradientGen      *TextualGradientGenerator
	contrastAna      *ContrastiveAnalyzer
	geneticSearch    *GeneticPromptSearch
	promptMem        *PromptMemory
	errorMem         *ErrorPatternMemory
	maxBudget        int // 软上限 30K tokens/周期
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
// 调用方在构造后应显式调用 LoadFromDB 恢复历史状态。
func NewPromptOptimizerWithDB(provider protocol.Provider, versionStore *PromptVersionStore, db *sql.DB, maxBudget int) *PromptOptimizer {
	po := NewPromptOptimizer(provider, versionStore, maxBudget)
	if db != nil {
		po.heuristicsStore = NewHeuristicsStore(db)
	}
	return po
}

// AddAvoidRule 将错误规避规则注入 ErrorPatternMemory，并异步持久化到 fallacy_records。
// 由 self_improve.Engine 内环在收到 HeuristicGeneratedEvent 后调用。
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
	po.errorMem.patterns[id] = pat
	if po.heuristicsStore != nil {
		p := pat
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = po.heuristicsStore.SaveFallacy(ctx, p)
		}()
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
	po.promptMem.entries[taskType] = append(po.promptMem.entries[taskType], s)
	if po.heuristicsStore != nil {
		go func() {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = po.heuristicsStore.SaveHeuristic(saveCtx, taskType, s)
		}()
	}
}

// LoadFromDB 从 DB 恢复跨会话 heuristics 和 fallacy 规则到内存。
// 应在构造后、首次 Optimize 前调用；加载失败不阻断（返回错误供调用方记录）。
func (po *PromptOptimizer) LoadFromDB(ctx context.Context) error {
	if po.heuristicsStore == nil {
		return nil
	}
	heuristics, err := po.heuristicsStore.LoadHeuristics(ctx)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_optimizer: load heuristics", err)
	}
	for taskType, strategies := range heuristics {
		po.promptMem.entries[taskType] = append(po.promptMem.entries[taskType], strategies...)
	}

	fallacies, err := po.heuristicsStore.LoadFallacies(ctx)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_optimizer: load fallacies", err)
	}
	maps.Copy(po.errorMem.patterns, fallacies)
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
type PromptMemory struct {
	entries map[string][]*PromptStrategy
}

// ErrorPatternMemory 错误模式记忆（ContraPrompt 用）。
type ErrorPatternMemory struct {
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

// GetTopStrategies 返回指定 taskType 的 top-N 高分策略（MemAPO 查询）。
func (pm *PromptMemory) GetTopStrategies(taskType string, n int) []*PromptStrategy {
	strategies, ok := pm.entries[taskType]
	if !ok {
		return nil
	}
	sorted := make([]*PromptStrategy, len(strategies))
	copy(sorted, strategies)
	sortStrategiesByRate(sorted)
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

// GetAvoidRules 返回匹配 taskType 的 AvoidRule 列表。
// taskType 为空时返回全部规则（兼容冷启动场景）；
// TaskType 为空的 ErrorPattern（旧格式）视为全局规则，始终包含。
func (em *ErrorPatternMemory) GetAvoidRules(taskType string) []string {
	var rules []string
	for _, p := range em.patterns {
		if p.AvoidRule == "" {
			continue
		}
		if taskType == "" || p.TaskType == "" || p.TaskType == taskType {
			rules = append(rules, p.AvoidRule)
		}
	}
	return rules
}

// Generate 通过 LLM 生成文本梯度（失败 → 成功的优化方向）。
// provider nil 时回退到规则模板（离线/冷启动场景）。
func (tgg *TextualGradientGenerator) Generate(ctx context.Context, failedPrompt, succeededPrompt string) string {
	if failedPrompt == "" || succeededPrompt == "" {
		return ""
	}
	if tgg.provider == nil {
		// 离线 fallback：规则模板
		return "Improve the following prompt by learning from successful patterns:\n[SUCCESS]: " +
			trunc(succeededPrompt, 200) + "\n[TO IMPROVE]: " + trunc(failedPrompt, 200)
	}
	prompt := fmt.Sprintf(
		"You are a prompt optimization expert. A failed prompt and a successful prompt are given.\n"+
			"Analyze the key differences and generate an improved version of the failed prompt.\n\n"+
			"Failed prompt:\n%s\n\nSuccessful prompt:\n%s\n\n"+
			"Output ONLY the improved prompt text, no explanation.",
		trunc(failedPrompt, 500), trunc(succeededPrompt, 500),
	)
	resp, err := tgg.provider.Infer(ctx, &protocol.InferRequest{
		Messages:        []protocol.Message{{Role: "user", Content: prompt}},
		Temperature:     0.3,
		MaxTokens:       1024,
		ReasoningEffort: protocol.ReasoningEffortHigh, // 深思精益突变
	})
	if err != nil {
		// LLM 失败回退规则模板，不阻断流程
		return "Improve: " + trunc(failedPrompt, 200)
	}
	return resp.Content
}

// Analyze 通过 LLM 对比成功和失败轨迹，提取避免规则。
// provider nil 时返回空字符串（跳过该步骤）。
func (ca *ContrastiveAnalyzer) Analyze(ctx context.Context, successPrompt, failedPrompt string) string {
	if successPrompt == "" || failedPrompt == "" {
		return ""
	}
	if ca.provider == nil {
		return ""
	}
	prompt := fmt.Sprintf(
		"Compare these two prompts. The first succeeded, the second failed.\n"+
			"In one concise sentence, describe the key pattern to AVOID in the failed prompt.\n\n"+
			"Successful:\n%s\n\nFailed:\n%s\n\nOutput only the avoid-rule sentence.",
		trunc(successPrompt, 400), trunc(failedPrompt, 400),
	)
	resp, err := ca.provider.Infer(ctx, &protocol.InferRequest{
		Messages:        []protocol.Message{{Role: "user", Content: prompt}},
		Temperature:     0.1,
		MaxTokens:       256,
		ReasoningEffort: protocol.ReasoningEffortHigh, // 深入对比分析
	})
	if err != nil {
		return ""
	}
	return resp.Content
}

// Search 执行 Pareto 前沿搜索（MVP：按加权分降序近似）。
// 架构规约: 种群 8 × 5 代，早停 = 连续 2 代前沿无新非支配解。
func (gps *GeneticPromptSearch) Search(candidates []*PromptVersion) []*PromptVersion {
	if len(candidates) == 0 {
		return nil
	}
	pop := candidates
	if len(pop) > gps.populationSize {
		pop = pop[:gps.populationSize]
	}
	result := make([]*PromptVersion, len(pop))
	copy(result, pop)
	return sortByWeightedScore(result)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func findBestWorst(versions []*PromptVersion) (best, worst *PromptVersion) {
	best, worst = versions[0], versions[0]
	for _, v := range versions[1:] {
		if v.Score > best.Score {
			best = v
		}
		if v.Score < worst.Score {
			worst = v
		}
	}
	return
}

func sortByScore(vs []*PromptVersion) []*PromptVersion {
	for i := 0; i < len(vs)-1; i++ {
		for j := i + 1; j < len(vs); j++ {
			if vs[j].Score > vs[i].Score {
				vs[i], vs[j] = vs[j], vs[i]
			}
		}
	}
	return vs
}

// sortByWeightedScore Pareto 近似：0.6×Score + 0.4×(1/max(Cost,0.001))。
func sortByWeightedScore(vs []*PromptVersion) []*PromptVersion {
	score := func(v *PromptVersion) float64 {
		costInv := 1.0 / maxF64(v.Cost, 0.001)
		return 0.6*v.Score + 0.4*costInv
	}
	for i := 0; i < len(vs)-1; i++ {
		for j := i + 1; j < len(vs); j++ {
			if score(vs[j]) > score(vs[i]) {
				vs[i], vs[j] = vs[j], vs[i]
			}
		}
	}
	return vs
}

func sortStrategiesByRate(ss []*PromptStrategy) {
	for i := 0; i < len(ss)-1; i++ {
		for j := i + 1; j < len(ss); j++ {
			if ss[j].SuccessRate > ss[i].SuccessRate {
				ss[i], ss[j] = ss[j], ss[i]
			}
		}
	}
}

func joinStrings(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}

// trunc 截断字符串到指定字节数（UTF-8 安全近似）。
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func maxF64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// 编译期确认 perrors 被使用（避免 import 误删）。
var _ = perrors.CodeInternal
