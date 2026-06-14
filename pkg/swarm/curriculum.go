package swarm

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
	"github.com/polarisagi/polaris/pkg/substrate/policy"
)

// Auto-Curriculum 自动课程生成器。
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.2

const (
	maxCurriculumDifficulty = 0.85 // SurpriseIndex 硬上限
	maxPerSkill             = 3    // 每技能最多生成课程数
	maxPerCycle             = 10   // 每周期总上限
	freezeDuration          = 60 * time.Minute
)

// 危险命令黑名单（Phase 3 (b) 安全审查）。
var dangerousCommands = []string{
	"shell", "bash", "sh ", "/bin/", "exec ", "rm ", "dd ", "mkfs",
	"sudo", "chmod", "chown", "> /", "curl ", "wget ", "python -c",
	"eval(", "os.system", "subprocess",
}

// AutoCurriculumGenerator 空闲期自动生成边缘能力任务。
type AutoCurriculumGenerator struct {
	idleDetector *IdleDetector
	memf         *FallacyMemoryPool
	heuristics   *HeuristicsMemory
	taintGate    *policy.TaintGate
	sicCleaner   *policy.SICCleaner
	llmProvider  protocol.Provider // Tier1+：LLM 描述生成 + safety judge；nil 时降级模板

	// bb 由 WithBlackboard 注入，供 GenerateForEngine 适配方法使用（P1-5）。
	bb protocol.Blackboard

	// 连续失败冻结记录: sourceSkill → (failCount, frozenUntil)
	mu          sync.Mutex
	failCounts  map[string]int
	frozenUntil map[string]time.Time
}

// NewAutoCurriculumGenerator 创建课程生成器。
func NewAutoCurriculumGenerator(
	idle *IdleDetector,
	memf *FallacyMemoryPool,
	heuristics *HeuristicsMemory,
) *AutoCurriculumGenerator {
	return &AutoCurriculumGenerator{
		idleDetector: idle,
		memf:         memf,
		heuristics:   heuristics,
		taintGate:    &policy.TaintGate{},
		sicCleaner:   policy.NewSICCleaner(),
		failCounts:   make(map[string]int),
		frozenUntil:  make(map[string]time.Time),
	}
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
// self_improve.CurriculumGenerator 接口（P1-5）。
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

// Generate 实现 self_improve.CurriculumGenerator 接口。
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
//  1. OS 可用内存 > minFreeMB（调用 observability.ProbeAvailableMemoryMB）
//  2. Goroutine 数量 < maxGoroutines（近似 CPU 压力）
func (d *IdleDetector) IsIdle() bool {
	if observability.ProbeAvailableMemoryMB() < d.minFreeMB {
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

	// 步骤 2 — SkillGapAnalysis：从 HeuristicsMemory 找 50-90% 成功率的技能
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
		skillSamples := ag.generateDescriptions(skill, maxPerSkill)

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
			entry := &protocol.TaskEntry{
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

// skillGapAnalysis 从 HeuristicsMemory 筛选 50-90% 成功率的技能。
func (ag *AutoCurriculumGenerator) skillGapAnalysis(ctx context.Context) []string {
	if ag.heuristics == nil {
		return nil
	}
	// 查询 heuristics_memory 中各 task_type 的平均成功率
	rows, err := ag.heuristics.db.QueryContext(ctx, `
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
func (ag *AutoCurriculumGenerator) generateDescriptions(skill string, limit int) []*CurriculumSample {
	if ag.llmProvider != nil {
		if samples := ag.generateDescriptionsLLM(skill, limit); len(samples) > 0 {
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
func (ag *AutoCurriculumGenerator) generateDescriptionsLLM(skill string, limit int) []*CurriculumSample {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prompt := fmt.Sprintf(
		"Generate %d concise task descriptions for testing AI skill: %q.\n"+
			"Format: one description per line, each 10-20 words, covering edge cases and variations.\n"+
			"Output only the descriptions, no numbering.",
		limit, skill,
	)
	req := &protocol.InferRequest{
		Messages:    []protocol.Message{{Role: "user", Content: prompt}},
		MaxTokens:   256,
		Temperature: 0.8,
	}
	resp, err := ag.llmProvider.Infer(ctx, req.Messages, protocol.WithMaxTokens(req.MaxTokens))
	if err != nil || resp == nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(resp.Content), "\n")
	var samples []*CurriculumSample //nolint:prealloc
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || i >= limit {
			continue
		}
		samples = append(samples, &CurriculumSample{
			TaskDescription:    line,
			DifficultyEstimate: 0.65 + float64(i)*0.05,
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

	// (a) TaintGate：任务描述仅允许 TaintLow 写入 instruction 槽
	if err := ag.taintGate.CheckSlotAssignment(policy.SlotInstruction, 1 /* TaintLow */); err != nil {
		return false
	}

	// (b) 危险命令黑名单
	lower := strings.ToLower(desc)
	for _, bad := range dangerousCommands {
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

// llmJudgeSafe LLM-as-Judge 安全审查（Tier1+）。
// 调用 LLM 判断任务描述是否安全：返回 "SAFE"/"UNSAFE"。
// 超时或 LLM 错误时 fail-open（返回 true），防止 LLM 故障阻断全部课程生成。
func (ag *AutoCurriculumGenerator) llmJudgeSafe(ctx context.Context, desc string) bool {
	judgeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	prompt := fmt.Sprintf(
		"Safety check for AI training task: %q\n"+
			"Reply with exactly one word: SAFE or UNSAFE.\n"+
			"UNSAFE if the task involves: hacking, self-modification, data deletion, deception, or harm.",
		desc,
	)
	req := &protocol.InferRequest{
		Messages:    []protocol.Message{{Role: "user", Content: prompt}},
		MaxTokens:   8,
		Temperature: 0,
	}
	resp, err := ag.llmProvider.Infer(judgeCtx, req.Messages, protocol.WithMaxTokens(req.MaxTokens))
	if err != nil || resp == nil {
		return true // fail-open：LLM 故障不阻断课程生成
	}
	verdict := strings.TrimSpace(strings.ToUpper(resp.Content))
	return !strings.HasPrefix(verdict, "UNSAFE")
}

// isFrozen 检查技能是否处于冻结期。
func (ag *AutoCurriculumGenerator) isFrozen(skill string) bool {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if t, ok := ag.frozenUntil[skill]; ok && time.Now().Before(t) {
		return true
	}
	return false
}

// FoundingAnchorMeta 提供创始行为锚点元数据（解耦依赖）。
type FoundingAnchorMeta interface {
	GetCreatedAt() int64
	GetTaskCount() int
}

// RedTeamRunner 执行红队演练并注入到 Holdout 评估集（解耦依赖）。
type RedTeamRunner interface {
	RunAndInject(ctx context.Context) error
}

// BackgroundTaskScheduler 后台调度器。
type BackgroundTaskScheduler struct {
	generator      *AutoCurriculumGenerator
	bb             protocol.Blackboard
	surpriseReader SurpriseReader
	foundingAnchor FoundingAnchorMeta // 可选；nil 时跳过周度漂移检查
	anchorDataDir  string             // ~/.polarisagi/polaris/
	redTeam        RedTeamRunner      // 可选；nil 时跳过 24h 红队探测
}

// SurpriseReader 读取当前系统 SurpriseIndex。
type SurpriseReader interface {
	CurrentSurprise() float64
}

// NewBackgroundTaskScheduler 创建后台调度器。
func NewBackgroundTaskScheduler(gen *AutoCurriculumGenerator, bb protocol.Blackboard) *BackgroundTaskScheduler {
	return &BackgroundTaskScheduler{generator: gen, bb: bb}
}

// InjectSurpriseReader 注入 SurpriseIndex 读取器（可选——nil 时使用 0.5 默认值）。
func (b *BackgroundTaskScheduler) InjectSurpriseReader(r SurpriseReader) {
	b.surpriseReader = r
}

// InjectFoundingAnchor 注入创始行为锚点（可选）。
func (b *BackgroundTaskScheduler) InjectFoundingAnchor(anchor FoundingAnchorMeta, dataDir string) {
	b.foundingAnchor = anchor
	b.anchorDataDir = dataDir
}

// InjectRedTeamProtocol 注入 Red Team 协议（可选）。
func (b *BackgroundTaskScheduler) InjectRedTeamProtocol(r RedTeamRunner) {
	b.redTeam = r
}

// readSurprise 读取当前系统 SurpriseIndex。
// 优先级: surpriseReader → 0.5 默认值。
func (b *BackgroundTaskScheduler) readSurprise() float64 {
	if b.surpriseReader != nil {
		return b.surpriseReader.CurrentSurprise()
	}
	return 0.5
}

// Start 启动后台守护协程（2 分钟轮询）。
func (b *BackgroundTaskScheduler) Start(ctx context.Context) {
	// 保持原有：2 分钟 AutoCurriculum 生成（不修改）
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				si := b.readSurprise()
				b.generator.Generate(ctx, b.bb, si)
			}
		}
	}()

	// 新增：7 天 FoundingAnchor 漂移检查（V8-S3）
	if b.foundingAnchor != nil {
		go func() {
			ticker := time.NewTicker(7 * 24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					b.runFoundingAnchorCheck(ctx)
				}
			}
		}()
	}

	// 新增：24 小时 Red Team 常态化探测（V8-S1）
	if b.redTeam != nil {
		go func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := b.redTeam.RunAndInject(ctx); err != nil {
						slog.Error("red team: run and inject failed", "err", err)
					}
				}
			}
		}()
	}
}

// runFoundingAnchorCheck 执行周度创始锚点漂移检查。
// TODO: 完整实现需注入 TrajectoryStore 读取近期轨迹。MVP 阶段记录 anchor 存在性。
func (b *BackgroundTaskScheduler) runFoundingAnchorCheck(_ context.Context) {
	if b.foundingAnchor == nil {
		return
	}
	// TODO: 注入 TrajectoryStore 后读取近期 100 条轨迹，调用 CompareWithAnchor
	slog.Info("founding anchor check: anchor present",
		"created_at", b.foundingAnchor.GetCreatedAt(),
		"task_count", b.foundingAnchor.GetTaskCount(),
	)
}
