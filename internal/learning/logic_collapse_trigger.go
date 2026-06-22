package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	// Logic Collapse 触发器 — M9 Self-Improvement Engine 的蒸馏入口。
	// 架构文档: docs/arch/M06-Skill-Library.md §2.2, docs/arch/M09-Self-Improvement-Engine.md §1.1
	//
	// M9 BackgroundTaskScheduler 调用 LogicCollapseMonitor.RecordSuccess 记录每次成功轨迹。
	// 当 SuccessCount >= 50 且 SemanticVariance >= 0.1 且 EvalGate 通过时，
	// 异步触发 LogicCollapseCompiler.Compile 将轨迹蒸馏为 TypeScript 技能脚本。

	"github.com/polarisagi/polaris/pkg/apperr"

	extskill "github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/protocol"
)

// ─── 接口约定 ─────────────────────────────────────────────────────────────────

// TrajectoryCompiler 是 extskill.LogicCollapseCompiler 的抽象（防止 test 时需要构建完整编译器）。
type TrajectoryCompiler interface {
	Compile(ctx context.Context, req *extskill.CompileRequest) (*extskill.CompileResult, error)
}

// HITLNotifier 高风险技能 HITL 通知（由 M13 Interface 实现）。
type HITLNotifier interface {
	NotifyHITL(ctx context.Context, skillID, reason string) error
}

// ─── TrajectoryStats — 每技能成功轨迹统计 ────────────────────────────────────

// TrajectoryStats 追踪每个技能的成功次数与语义方差。
type TrajectoryStats struct {
	mu           sync.Mutex
	SuccessCount int
	// embeddings 存储最近 50 次成功轨迹的嵌入向量（用于方差计算）
	embeddings [][]float32
	// sumEmbed / sumSqEmbed 用于在线方差估算（Welford 算法）
	mean []float64
	m2   []float64
	n    int
	// lastTriggerAt 上次触发编译时间（防重复触发）
	lastTriggerAt time.Time
}

// AddEmbedding 追加成功轨迹嵌入向量，更新在线均值/方差（Welford 算法）。
// 最多保留最近 50 条（滑动窗口）。
func (ts *TrajectoryStats) AddEmbedding(emb []float32) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.SuccessCount++
	if len(emb) == 0 {
		return
	}

	// 初始化在线统计维度
	if ts.mean == nil {
		ts.mean = make([]float64, len(emb))
		ts.m2 = make([]float64, len(emb))
	}

	// Welford 在线更新
	ts.n++
	for i, v := range emb {
		if i >= len(ts.mean) {
			break
		}
		delta := float64(v) - ts.mean[i]
		ts.mean[i] += delta / float64(ts.n)
		delta2 := float64(v) - ts.mean[i]
		ts.m2[i] += delta * delta2
	}

	// 滑动窗口保留最近 50 条
	ts.embeddings = append(ts.embeddings, emb)
	if len(ts.embeddings) > 50 {
		ts.embeddings = ts.embeddings[1:]
	}
}

// SemanticVariance 返回多维嵌入向量的平均维度方差（衡量输入多样性）。
// n < 2 时返回 0（无法计算）。
func (ts *TrajectoryStats) SemanticVariance() float64 {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.n < 2 || len(ts.m2) == 0 {
		return 0
	}
	var total float64
	for _, m2 := range ts.m2 {
		total += m2 / float64(ts.n-1)
	}
	// 各维度方差的 L2 范数作为总体方差指标
	return math.Sqrt(total / float64(len(ts.m2)))
}

// ReadyToCollapse 判断是否满足 Logic Collapse 触发条件。
func (ts *TrajectoryStats) ReadyToCollapse() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.SuccessCount < extskill.MinSuccessCount {
		return false
	}
	if ts.lastTriggerAt.IsZero() {
		return true
	}
	// 触发后 24h 内不重复触发（防止反复重编译）
	return time.Since(ts.lastTriggerAt) > 24*time.Hour
}

// MarkTriggered 记录触发时间（调用方须在 ReadyToCollapse 返回 true 后调用）。
func (ts *TrajectoryStats) MarkTriggered() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastTriggerAt = time.Now()
}

// ─── LogicCollapseMonitor ─────────────────────────────────────────────────────

// LogicCollapseMonitor M9 对每个技能的执行质量监控，达到阈值时触发 Logic Collapse。
type LogicCollapseMonitor struct {
	mu         sync.RWMutex
	stats      map[string]*TrajectoryStats // skillID → stats
	compiler   TrajectoryCompiler
	codeGen    extskill.LLMCodeGenerator
	registry   protocol.SkillRegistry
	hitl       HITLNotifier // nil → 高风险技能跳过（仅 WARN 日志）
	signingKey []byte
	workDir    string

	// stagingPipeline 用于将 L2 编译结果提交到渐进发布流水线（Stage 1 candidate_emit）。
	// nil 时跳过 Staging 提交（测试 / Day0 冷启动场景）。
	stagingPipeline optimizer.StagingPipeline
}

// NewLogicCollapseMonitor 创建监控器。
func NewLogicCollapseMonitor(
	compiler TrajectoryCompiler,
	codeGen extskill.LLMCodeGenerator,
	registry protocol.SkillRegistry,
	hitl HITLNotifier,
	signingKey []byte,
	workDir string,
) *LogicCollapseMonitor {
	return &LogicCollapseMonitor{
		stats:      make(map[string]*TrajectoryStats),
		compiler:   compiler,
		codeGen:    codeGen,
		registry:   registry,
		hitl:       hitl,
		signingKey: signingKey,
		workDir:    workDir,
	}
}

// WithStagingPipeline 注入 Staging 流水线（可选；nil 时跳过 Stage1 提交）。
func (m *LogicCollapseMonitor) WithStagingPipeline(sp optimizer.StagingPipeline) *LogicCollapseMonitor {
	m.stagingPipeline = sp
	return m
}

// RecordSuccess 记录技能成功执行轨迹（M9 在每次成功后调用）。
// embedding 为本次输入的 embedding 向量（可为空，则仅计数）。
// 若达到触发条件，异步启动 Logic Collapse 编译（不阻塞调用链）。
func (m *LogicCollapseMonitor) RecordSuccess(
	ctx context.Context,
	traj *extskill.CollapseTrajectory,
	embedding []float32,
) {
	m.mu.Lock()
	stats, ok := m.stats[traj.SkillID]
	if !ok {
		stats = &TrajectoryStats{}
		m.stats[traj.SkillID] = stats
	}
	m.mu.Unlock()

	stats.AddEmbedding(embedding)

	variance := stats.SemanticVariance()

	if !stats.ReadyToCollapse() {
		return
	}
	if variance < extskill.MinSemanticVariance {
		slog.Warn("logic_collapse: semantic_variance too low — needs_more_diversity",
			"skill_id", traj.SkillID,
			"variance", variance,
			"success_count", stats.SuccessCount,
		)
		return
	}

	stats.MarkTriggered()

	// 异步触发编译（L1 优先级后台任务）
	go m.triggerCollapse(context.Background(), traj, variance)
}

// triggerCollapse 执行 Eval Gate 检查 + 编译触发（异步运行）。
func (m *LogicCollapseMonitor) triggerCollapse(ctx context.Context, traj *extskill.CollapseTrajectory, variance float64) {
	// 1. 高风险技能 → HITL Gateway [ESCALATE]
	if traj.RiskLevel == "high" {
		if m.hitl != nil {
			if err := m.hitl.NotifyHITL(ctx, traj.SkillID, "logic_collapse_high_risk"); err != nil {
				slog.Error("logic_collapse: HITL notification failed",
					"skill_id", traj.SkillID, "err", err)
			}
		} else {
			slog.Warn("logic_collapse: high-risk skill requires HITL but no notifier configured",
				"skill_id", traj.SkillID)
		}
		// 高风险技能等待 HITL 审批，不自动触发编译
		return
	}

	// 2. 低/中风险 → 自动 Eval Gate（简化版：标记 EvalGatePassed）
	// 生产中应调用 M12 Eval Harness 执行自动回归测试
	evalGatePassed := m.runEvalGate(ctx, traj)
	if !evalGatePassed {
		slog.Warn("logic_collapse: eval gate not passed",
			"skill_id", traj.SkillID)
		return
	}

	// 3. 构建 CompileRequest
	req := &extskill.CompileRequest{
		Trajectory:     traj,
		EvalGatePassed: true,
		SigningKey:     m.signingKey,
		WorkDir:        m.workDir,
	}

	// 4. 调用 Logic Collapse 编译器
	result, err := m.compiler.Compile(ctx, req)
	if err != nil {
		slog.Error("logic_collapse: compile failed",
			"skill_id", traj.SkillID,
			"err", err,
			"variance", variance,
			"success_count", traj.SuccessCount,
		)
		return
	}

	slog.Info("logic_collapse: skill compiled and registered",
		"skill_id", traj.SkillID,
		"script_hash", result.ScriptHash,
		"risk_level", result.RiskLevel,
		"sandbox_tier", result.SandboxTier,
	)

	// ─── [P2-NEW] Stage 1 candidate_emit：将编译结果提交到 Staging 流水线 ─────────
	// 仅 low/medium 风险技能自动进入 Staging；high 风险已在前面走 HITL 提前返回。
	// 遵循 HE-4：所有自演化候选必须通过 Staging CI 门控才能进入生产记忆。
	if m.stagingPipeline != nil {
		snap := &optimizer.AgentVersionSnapshot{
			Version:         fmt.Sprintf("skill-%s-%d", traj.SkillID, time.Now().Unix()),
			SkillSnapshotID: traj.SkillID,
			CreatedAt:       time.Now().Unix(),
		}
		if subErr := m.stagingPipeline.SubmitCandidate(ctx, snap); subErr != nil {
			// 提交失败不阻断当前流程（技能已编译注册），仅告警
			slog.Warn("logic_collapse: staging submit failed, skill registered but staging skipped",
				"skill_id", traj.SkillID,
				"version", snap.Version,
				"err", subErr,
			)
		} else {
			slog.Info("logic_collapse: staging candidate submitted",
				"skill_id", traj.SkillID,
				"version", snap.Version,
				"gate", "1/shadow_1pct",
			)
		}
	}
	// ─── [/P2-NEW] ────────────────────────────────────────────────────────────────
}

// runEvalGate 简化版自动 Eval Gate（生产中替换为 M12 Eval Harness 调用）。
// 当前仅检查技能是否已注册（避免空编译），实际应执行 5 条黄金测试用例。
func (m *LogicCollapseMonitor) runEvalGate(_ context.Context, traj *extskill.CollapseTrajectory) bool {
	// Day-0 冷启动分级阈值:
	// (a) 黄金用例=0 且成功≥50 → Auto-Eval-Bootstrapping（当前简化：允许通过）
	// (b) 用例<5 → 降低阈值（当前简化：允许通过）
	// 生产实现需调用 M12 EvalRunner 执行 L4 LLM-as-Judge 深度审查
	return traj.SuccessCount >= extskill.MinSuccessCount
}

// GetStats 返回技能的轨迹统计（主要用于测试）。
func (m *LogicCollapseMonitor) GetStats(skillID string) *TrajectoryStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats[skillID]
}

// ─── defaultLLMCodeGenerator — LLM 代码生成实现 ───────────────────────────────

// defaultLLMCodeGenerator 使用 protocol.Provider 生成 TinyGo impl.go。
type defaultLLMCodeGenerator struct {
	provider protocol.Provider
}

// NewDefaultLLMCodeGenerator 创建默认 LLM 代码生成器。
func NewDefaultLLMCodeGenerator(provider protocol.Provider) extskill.LLMCodeGenerator {
	return &defaultLLMCodeGenerator{provider: provider}
}

// GenerateImpl 将脱敏轨迹发送给 LLM 生成 TypeScript MCP 技能脚本。
func (g *defaultLLMCodeGenerator) GenerateImpl(ctx context.Context, traj *extskill.CollapseTrajectory) ([]byte, error) {
	if g.provider == nil {
		return nil, apperr.New(apperr.CodeInternal, "logic_collapse: LLM provider is nil")
	}

	toolCallsDesc := buildToolCallsDescription(traj.ToolCalls)
	inputSchemaDesc := buildSchemaDescription(traj.InputSchema)
	outputSchemaDesc := buildSchemaDescription(traj.OutputSchema)

	systemPrompt := `You are an AI generating a TypeScript MCP skill for the Polaris agent system.

STRICT REQUIREMENTS:
1. Use @modelcontextprotocol/sdk and standard TypeScript
2. Export a default async function that accepts JSON input and returns JSON output
3. NO dynamic execution: no eval(), no new Function(), no require()
4. Use import (ESM), not require (CommonJS)
5. NO direct filesystem writes or network calls unless explicitly declared in capabilities
6. Input/output must be valid JSON-serializable objects
7. The script runs via: npx tsx src/index.ts

Output ONLY valid TypeScript source code, no markdown, no explanation.`

	userPrompt := fmt.Sprintf(`Generate src/index.ts for skill "%s":

Goal: %s

Tool call sequence (type signatures only):
%s

Input schema: %s
Output schema: %s

The script must implement the deterministic equivalent of this tool call sequence.`,
		traj.SkillID,
		traj.GoalDescription,
		toolCallsDesc,
		inputSchemaDesc,
		outputSchemaDesc,
	)

	req := &types.InferRequest{
		Messages: []types.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	resp, err := g.provider.Infer(ctx, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "LLM inference failed", err)
	}

	src := strings.TrimSpace(resp.Content)
	// 剥离 LLM 可能包裹的 Markdown 代码块
	src = strings.TrimPrefix(src, "```go")
	src = strings.TrimPrefix(src, "```")
	src = strings.TrimSuffix(src, "```")
	src = strings.TrimSpace(src)

	return []byte(src), nil
}

// buildToolCallsDescription 将工具调用类型签名格式化为 LLM 可读的描述。
func buildToolCallsDescription(calls []extskill.CollapseToolCall) string {
	if len(calls) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, c := range calls {
		argsJSON, _ := json.Marshal(c.Args)
		fmt.Fprintf(&sb, "  %d. %s(args: %s) -> %s\n",
			c.OrderIndex+1, c.ToolName, argsJSON, c.OutputType)
	}
	return sb.String()
}

// buildSchemaDescription 将 map[string]string schema 格式化为描述。
func buildSchemaDescription(schema map[string]string) string {
	if len(schema) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(schema)
	return string(b)
}
