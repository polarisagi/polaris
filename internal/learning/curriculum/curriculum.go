package curriculum

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// Auto-Curriculum 自动课程生成器。
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.2
//
// llmJudgeSafe/isFrozen 与 BackgroundTaskScheduler（周度锚点漂移检查/红队探测）
// 见 curriculum_scheduler.go（R7 拆分）。

const (
	maxCurriculumDifficulty = 0.85 // SurpriseIndex 硬上限
	maxPerSkill             = 3    // 每技能最多生成课程数
	maxPerCycle             = 10   // 每周期总上限
	freezeDuration          = 60 * time.Minute
)

// dangerousCommands 危险命令黑名单（Phase 3 (b) 安全审查），只读。
func dangerousCommands() []string {
	return []string{
		"shell", "bash", "sh ", "/bin/", "exec ", "rm ", "dd ", "mkfs",
		"sudo", "chmod", "chown", "> /", "curl ", "wget ", "python -c",
		"eval(", "os.system", "subprocess",
	}
}

// AutoCurriculumGenerator 空闲期自动生成边缘能力任务。
type AutoCurriculumGenerator struct {
	idleDetector *IdleDetector
	memf         *optimizer.FallacyMemoryPool
	heuristics   *optimizer.HeuristicsMemory
	taintGate    *taint.TaintGate
	sicCleaner   *guard.SICCleaner
	llmProvider  protocol.Provider // Tier1+：LLM 描述生成 + safety judge；nil 时降级模板

	// bb 由 WithBlackboard 注入，供 GenerateForEngine 适配方法使用（P1-5）。
	bb protocol.Blackboard

	// 连续失败冻结记录: sourceSkill → (failCount, frozenUntil)
	mu          sync.Mutex
	failCounts  map[string]int
	frozenUntil map[string]time.Time

	fitnessEval *SQLFitnessEvaluator // 可 nil；nil 时跳过 SQL 预筛
}

// NewAutoCurriculumGenerator 创建课程生成器。
func NewAutoCurriculumGenerator(
	idle *IdleDetector,
	memf *optimizer.FallacyMemoryPool,
	heuristics *optimizer.HeuristicsMemory,
) *AutoCurriculumGenerator {
	return &AutoCurriculumGenerator{
		idleDetector: idle,
		memf:         memf,
		heuristics:   heuristics,
		taintGate:    &taint.TaintGate{},
		sicCleaner:   guard.NewSICCleaner(),
		failCounts:   make(map[string]int),
		frozenUntil:  make(map[string]time.Time),
	}
}

// WithFitnessEval 注入 SQL 适应度预筛器。
func (ag *AutoCurriculumGenerator) WithFitnessEval(fe *SQLFitnessEvaluator) *AutoCurriculumGenerator {
	ag.fitnessEval = fe
	return ag
}

// InjectLLMProvider 注入 LLM Provider（Tier1+）。
func (ag *AutoCurriculumGenerator) InjectLLMProvider(p protocol.Provider) {
	ag.llmProvider = p
}

// WithBlackboard 注入 Blackboard，供 CurriculumGeneratorAdapter 使用（P1-5）。
// 返回自身支持链式调用。
func (ag *AutoCurriculumGenerator) WithBlackboard(bb protocol.Blackboard) *AutoCurriculumGenerator {
	ag.bb = bb
	return ag
}

// CurriculumGeneratorAdapter 将 AutoCurriculumGenerator 适配为
// learning.CurriculumGenerator 接口（P1-5）。
//
// 原因：AutoCurriculumGenerator.Generate 签名为 (ctx, bb, surpriseIndex) []*CurriculumSample，
// 与接口要求的 (ctx, surpriseIndex) error 不兼容。采用适配器而非修改原方法，
// 保持 BackgroundTaskScheduler 调用路径不变（最小改动原则）。
type CurriculumGeneratorAdapter struct {
	gen *AutoCurriculumGenerator
}

// NewCurriculumGeneratorAdapter 创建适配器；gen 必须已通过 WithBlackboard 注入 bb。
func NewCurriculumGeneratorAdapter(gen *AutoCurriculumGenerator) *CurriculumGeneratorAdapter {
	return &CurriculumGeneratorAdapter{gen: gen}
}

// Generate 实现 learning.CurriculumGenerator 接口。
// bb 为空时静默跳过（Tier-0 降级：不因 Blackboard 缺失阻断 Engine 启动）。
func (a *CurriculumGeneratorAdapter) Generate(ctx context.Context, surpriseIndex float64) error {
	if a.gen.bb == nil {
		return nil
	}
	a.gen.Generate(ctx, a.gen.bb, surpriseIndex)
	return nil
}

// IdleDetector 空闲检测器（可用内存 > 阈值 + Goroutine 数量适中）。
type IdleDetector struct {
	// minFreeMB 可用内存低水位线：低于此值视为非空闲，拒绝启动课程生成。
	// Tier-0 floor = 512MB（硬件门控，8GB 机器的安全边际）。
	minFreeMB uint64
	// maxGoroutines Goroutine 数量硬上限；超过则认为系统繁忙。
	maxGoroutines int
}

// NewIdleDetector 创建空闲检测器。
func NewIdleDetector() *IdleDetector {
	return &IdleDetector{
		minFreeMB:     512,
		maxGoroutines: 200,
	}
}

// IsIdle 判断系统是否满足课程生成的空闲条件：
//  1. OS 可用内存 > minFreeMB（调用 probe.ProbeAvailableMemoryMB）
//  2. Goroutine 数量 < maxGoroutines（近似 CPU 压力）
func (d *IdleDetector) IsIdle() bool {
	if probe.ProbeAvailableMemoryMB() < d.minFreeMB {
		return false
	}
	return runtime.NumGoroutine() < d.maxGoroutines
}

// CurriculumSample 课程任务样本。
type CurriculumSample struct {
	TaskDescription    string
	DifficultyEstimate float64
	SourceSkill        string
}

// Generate 生成课程任务并经四阶段安全审查后投递到 Blackboard。
// 9 步流程 + 4 阶段安全审查（架构文档 §2.2）。
func (ag *AutoCurriculumGenerator) Generate(ctx context.Context, bb protocol.Blackboard, currentSurpriseIndex float64) []*CurriculumSample {
	// 步骤 1 — 空闲检测
	if !ag.idleDetector.IsIdle() {
		return nil
	}

	// 步骤 2 — SkillGapAnalysis：从 optimizer.HeuristicsMemory 找 50-90% 成功率的技能
	candidates := ag.skillGapAnalysis(ctx)
	if len(candidates) == 0 {
		// 无候选技能时生成探索性兜底任务
		candidates = []string{"general_exploration"}
	}

	// 步骤 3 — MaxCurriculumDifficulty 硬上限：SurpriseIndex ≤ 0.85
	if currentSurpriseIndex > maxCurriculumDifficulty {
		return nil // 系统当前负荷过高，跳过课程生成
	}

	var posted []*CurriculumSample
	cycleCount := 0

	for _, skill := range candidates {
		if cycleCount >= maxPerCycle {
			break
		}

		// 步骤 4 — 连续失败冻结检查
		if ag.isFrozen(skill) {
			continue
		}

		// 步骤 5 — 生成课程描述（MVP：模板生成，Tier 1+ 替换为 LLM）
		skillSamples := ag.generateDescriptions(skill, maxPerSkill, currentSurpriseIndex)

		for _, sample := range skillSamples {
			if cycleCount >= maxPerCycle {
				break
			}

			// 步骤 6 — 四阶段安全审查
			if !ag.passSafetyAudit(ctx, sample) {
				continue
			}

			// 步骤 7 — 投递到 Blackboard（priority=3，低优先级）
			taskPayload := []byte(fmt.Sprintf(
				`{"type":"auto_curriculum","skill":%q,"desc":%q,"difficulty":%.2f}`,
				skill, sample.TaskDescription, sample.DifficultyEstimate,
			))
			entry := &types.TaskEntry{
				ID:            fmt.Sprintf("ac_%s_%d", skill, time.Now().UnixNano()),
				Type:          skill,
				Priority:      3,
				Intent:        taskPayload,
				Namespace:     fmt.Sprintf("curriculum_%s", skill),
				ToolWhitelist: []string{"read_file", "list_dir"},
				CreatedAt:     time.Now().Unix(),
			}
			if err := bb.PostTask(ctx, entry); err == nil {
				posted = append(posted, sample)
				cycleCount++
			}
		}
	}

	return posted
}

// ReportResult 记录课程任务结果，用于冻结计数更新。
// 成功 → 重置冻结计数；失败 → 递增，≥3 次触发 60min 冻结。
func (ag *AutoCurriculumGenerator) ReportResult(skill string, success bool) {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if success {
		ag.failCounts[skill] = 0
		return
	}
	ag.failCounts[skill]++
	if ag.failCounts[skill] >= 3 {
		ag.frozenUntil[skill] = time.Now().Add(freezeDuration)
		ag.failCounts[skill] = 0 // 重置计数，冻结期结束后重新计数
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// skillGapAnalysis 从 optimizer.HeuristicsMemory 筛选 50-90% 成功率的技能。
func (ag *AutoCurriculumGenerator) skillGapAnalysis(ctx context.Context) []string {
	if ag.heuristics == nil {
		return nil
	}
	// 查询 heuristics_memory 中各 task_type 的平均成功率
	rows, err := ag.heuristics.DB.QueryContext(ctx, `
		SELECT task_type, AVG(success_rate) as avg_rate
		FROM heuristics_memory
		GROUP BY task_type
		HAVING avg_rate >= 0.5 AND avg_rate <= 0.9
		LIMIT 5
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var skills []string
	for rows.Next() {
		var taskType string
		var rate float64
		if err := rows.Scan(&taskType, &rate); err == nil {
			skills = append(skills, taskType)
		}
	}
	return skills
}

// generateDescriptions 生成课程任务描述。
// Tier1+（llmProvider 已注入）：调用 LLM 生成多样性描述；Tier0：模板降级。
func (ag *AutoCurriculumGenerator) generateDescriptions(skill string, limit int, targetDifficulty float64) []*CurriculumSample {
	if ag.llmProvider != nil {
		if samples := ag.generateDescriptionsLLM(skill, limit, targetDifficulty); len(samples) > 0 {
			return samples
		}
	}
	// 离线/故障回退：模板生成
	templates := []string{
		"explore edge cases for %s with complex nested inputs",
		"handle error conditions in %s gracefully",
		"optimize performance of %s under high concurrency",
	}
	var samples []*CurriculumSample //nolint:prealloc
	for i, tmpl := range templates {
		if i >= limit {
			break
		}
		samples = append(samples, &CurriculumSample{
			TaskDescription:    fmt.Sprintf(tmpl, skill),
			DifficultyEstimate: 0.6 + float64(i)*0.1,
			SourceSkill:        skill,
		})
	}
	return samples
}

// generateDescriptionsLLM 通过 LLM 生成多样化课程描述（Tier1+）。
func (ag *AutoCurriculumGenerator) generateDescriptionsLLM(skill string, limit int, targetDifficulty float64) []*CurriculumSample {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prompt := fmt.Sprintf(
		"Generate %d concise task descriptions for testing AI skill: %q.\n"+
			"Format: one description per line, each 10-20 words, covering edge cases and variations.\n"+
			"Output only the descriptions, no numbering.",
		limit, skill,
	)
	req := &types.InferRequest{
		Messages:    []types.Message{{Role: "user", Content: prompt}},
		MaxTokens:   256,
		Temperature: 0.8,
	}
	resp, err := safecall.Infer(ctx, ag.llmProvider, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil || resp == nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(resp.Content), "\n")
	var samples []*CurriculumSample //nolint:prealloc
	step := 0.1 / float64(max(limit-1, 1))
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || i >= limit {
			continue
		}
		samples = append(samples, &CurriculumSample{
			TaskDescription:    line,
			DifficultyEstimate: clamp(targetDifficulty-0.05+float64(i)*step, 0.1, 0.95),
			SourceSkill:        skill,
		})
	}
	return samples
}

// passSafetyAudit 执行四阶段安全审查。
// (a) TaintGate  (b) 黑名单  (c) SIC  (d) LLM-Judge
// 任一阶段拒绝 → 返回 false。
func (ag *AutoCurriculumGenerator) passSafetyAudit(ctx context.Context, sample *CurriculumSample) bool {
	desc := sample.TaskDescription

	// SQL 适应度预筛：history 充足的技能直接淘汰低质量样本，无需 LLM 调用
	if ag.fitnessEval != nil {
		// 从 desc 中提取 skill_id（约定：desc 格式为 "skill:<id>:..." 或直接用 desc 哈希）
		// 当前阶段：用 desc 本身作为 skill_id 查询（无匹配时 EvaluateFitness 返回 -1）
		if fitness := ag.fitnessEval.EvaluateFitness(ctx, desc); fitness >= 0 && fitness < 0.5 {
			slog.Debug("curriculum: sql fitness pre-filter rejected sample",
				"fitness", fitness, "skill_id", desc[:min(len(desc), 32)])
			return false
		}
	}

	// (a) TaintGate：任务描述仅允许 TaintLow 写入 instruction 槽
	if err := ag.taintGate.CheckSlotAssignment(taint.SlotInstruction, 1 /* TaintLow */); err != nil {
		return false
	}

	// (b) 危险命令黑名单
	lower := strings.ToLower(desc)
	for _, bad := range dangerousCommands() {
		if strings.Contains(lower, bad) {
			return false
		}
	}

	// (c) SIC 指令清洗：检测间接 prompt injection
	if ag.sicCleaner != nil {
		if _, err := ag.sicCleaner.CleanInstructions(ctx, desc); err != nil {
			// ErrUncleanableContent → 拒绝
			return false
		}
	}

	// (d) LLM-as-Judge（Tier1+：调用 LLM 做 safety judge；Tier0 pass-through）
	if ag.llmProvider != nil {
		return ag.llmJudgeSafe(ctx, desc)
	}
	return true
}
