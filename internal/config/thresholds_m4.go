package config

// 2026-10-28 从 thresholds.go 拆分（Test_inv_FileLineLimit R7 行数上限，按模块职责拆出 M4 内核阈值），纯搬运无行为变更。

import (
	"math"
	"os"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type M4KernelThresholds struct {
	MaxReplanAttempts             int     `toml:"max_replan_attempts"`            // 3
	DefaultBudget                 int     `toml:"default_budget"`                 // 50000
	MaxSteps                      int     `toml:"max_steps"`                      // 24
	Tier0MaxConcurrent            int     `toml:"tier0_max_concurrent"`           // 4 — 同 max_concurrent_nodes
	SuspendIdleThresholdMin       int     `toml:"suspend_idle_threshold_minutes"` // 5
	PlanDAGMaxNodes               int     `toml:"plan_dag.max_nodes"`             // 50
	PlanDAGMaxDepth               int     `toml:"plan_dag.max_depth"`             // 10
	L3WatchdogMaxPerHour          int     `toml:"l3_watchdog.max_per_hour"`       // 10
	WorldModelSkipThreshold       float64 `toml:"world_model.skip_threshold"`     // 0.8
	SnapshotIntervalSteps         int     `toml:"snapshot.interval_steps"`        // 1000
	SnapshotRetentionCount        int     `toml:"snapshot.retention_count"`       // 5
	ReplanExtensionActivationSecs int     `toml:"replan_extension_activation_s"`  // 3
	SurpriseHintThreshold         float64 `toml:"surprise_hint_threshold"`        // 0.6

	// PRM（ProcessRewardModel）S_PLAN 候选 DAG 选优，见 docs/arch/M04-Agent-Kernel.md §4.6。
	// 默认关闭须显式开启（文档原文）：并发多候选打分会成倍增加 token 消耗，Operator 需明确知情后开启。
	PRMEnabled        bool    `toml:"prm.enabled"`         // false
	PRMComplexityGate float64 `toml:"prm.complexity_gate"` // 0.5 — 任务复杂度低于此值直接跳过，零额外开销
	PRMMaxCandidates  int     `toml:"prm.max_candidates"`  // 3 — 文档: "研究数据显示 3 候选 ROI 最优"
	PRMMinThreshold   float64 `toml:"prm.min_threshold"`   // 0.4 — 全部候选低于此分数时兜底取第一个候选
	PRMScorerModel    string  `toml:"prm.scorer_model"`    // "" — 留空则沿用 Provider 默认路由，不强制指定 budget-tier 模型名

	// 对话历史进入内核的上限（ADR-0098/0105；ADR-0120 决策四改为 token 预算）：越界一次丢弃前若干块，0=不限。
	ConversationHistoryMaxMessages int `toml:"conversation.history_max_messages"` // 0
	ConversationHistoryMaxBytes    int `toml:"conversation.history_max_bytes"`    // 0
	// ConversationHistoryMaxTokens 历史 token 预算（ADR-0120 决策四）：超出时一次丢到一半。
	ConversationHistoryMaxTokens int `toml:"conversation.history_max_tokens"` // 48000

	// PromptPhaseContractsInCore 阶段契约库并入稳定核（ADR-0105 决策九）：true 且存在 ImmutableCore 时，
	// 四阶段契约（PERCEIVE/PLAN/REFLECT/RESPOND）整体渲染进 L0，各阶段 L3 只写一条「当前阶段」选择器，
	// 四阶段 L0..L2 字节一致、L3 缩到几十字节；false 回到决策一的 L3 全文模板（回合契约评测不达标时的回退）。
	PromptPhaseContractsInCore bool `toml:"prompt.phase_contracts_in_core"` // true

	// 各阶段思考档位（ADR-0101 决策三）："" = 沿用 Provider 默认（DeepSeek 省略即 high），
	// 其余取 disabled/low/high/max。推理 token 按输出价计费：结构化分类阶段无需深度思考；
	// 首轮规划不再因用户输入恒为 TaintHigh 而恒为 max；升档由 metrics.SelectPlanTier 按失败成因决定（ADR-0102 决策六）。
	ThinkingPerceive    string `toml:"thinking.perceive"`     // "low"
	ThinkingPlanInitial string `toml:"thinking.plan_initial"` // "high"
	ThinkingReflect     string `toml:"thinking.reflect"`      // "low"
	ThinkingRespond     string `toml:"thinking.respond"`      // ""

	// 各阶段模型池（ADR-0101 决策七）：default=便宜档（flash/mini/haiku）、general=中档、
	// reasoning=贵档（pro/opus/o3）。日常回合全部走便宜档，plan_replan 为规划阶梯升级后的目标池（ADR-0102 决策六）；
	// 设 model_pool.plan_replan="default" 即完全不用贵档。
	ModelPoolPerceive    string `toml:"model_pool.perceive"`     // "default"
	ModelPoolPlanInitial string `toml:"model_pool.plan_initial"` // "default"
	ModelPoolPlanReplan  string `toml:"model_pool.plan_replan"`  // "reasoning"
	ModelPoolReflect     string `toml:"model_pool.reflect"`      // "default"
	ModelPoolRespond     string `toml:"model_pool.respond"`      // "default"
	ModelPoolValidate    string `toml:"model_pool.validate"`     // "default"
	// ModelPoolLoop agentic 主循环每轮调用的模型池（R5-07）。与 plan_initial 分开：规划池参与
	// 升级阶梯，主循环不升级；二者共用一个键会让调规划池的人无意间改掉默认模式的成本。
	ModelPoolLoop string `toml:"model_pool.loop"` // "default"

	// PlanReasoningComplexity 规划阶梯起点阈值（ADR-0102 决策六）：Perceive（LLM）给出的
	// TaskModel.Complexity ≥ 此值时首轮规划即用 model_pool.plan_replan 池 + ThinkingHigh；
	// 低于此值从 model_pool.plan_initial / thinking.plan_initial 起步，能力类失败再升级。
	PlanReasoningComplexity float64 `toml:"plan.reasoning_complexity"` // 0.7
	// ReflectSkipComplexity 反思跳过阈值（ADR-0102 决策四 4a）：首轮执行全部成功且
	// 0 < TaskModel.Complexity < 此值时不调 Reflect LLM，直接回复。0 = 关闭跳过。
	ReflectSkipComplexity float64 `toml:"reflect.skip_complexity"` // 0.4

	// CacheUniformTools 实验开关（ADR-0105 决策三，默认 false）：true 时 Perceive/Reflect/Respond
	// 也下发与 Plan 相同的 tools 并要求 Provider 不调用工具（tool_choice=none），四阶段 tools 前缀一致。
	CacheUniformTools bool `toml:"cache.uniform_tools"` // false

	// 记忆召回预算（ADR-0105 决策四）。召回段位于 L4，每回合都按未命中价计费，必须有上限。
	// RecallItemMaxChars 单条召回（情景/反思/L2 语义/RAG/画像）渲染后的字符（rune）上限，超出截断。
	RecallItemMaxChars int `toml:"recall.item_max_chars"` // 400
	// RecallMaxTokens 召回段总 token 上限；按 RRF 融合秩逐条装入，装不下即止（ADR-0105 决策十）。
	RecallMaxTokens int `toml:"recall.max_tokens"` // 1200
	// RecallMinScore L2 语义/RAG 命中的绝对分下限（低于丢弃）。0 = 关闭。L2 分是 SurrealDB BM25 原始分
	// （无界、随语料与查询漂移），RAG 分是检索融合分（RRF/重排，量纲随检索配置变），二者都不是归一化相似度，
	// 故默认不设绝对下限。
	RecallMinScore float64 `toml:"recall.min_score"` // 0
	// RecallMinScoreRatio L2 语义/RAG 命中相对本次最高分的比例下限（低于 ratio×top 丢弃，最高分命中恒保留）。
	// 尺度无关；0 = 关闭。ADR-0105 决策十起默认关闭：该比例无数据依据，来源间排序由 RRF 秩融合承担。
	RecallMinScoreRatio float64 `toml:"recall.min_score_ratio"` // 0

	// 来源权重（ADR-0105 决策十）：各来源按原生分排序后以 RRF（k=60）融合，来源权重乘在 RRF 分上。
	// 反思是已蒸馏的经验教训（单位 token 信息密度最高）、情景是与本用户/本项目直接相关的历史，
	// 故略高于 L2 语义与 RAG（外部/通用知识）；权重只决定"同秩时谁先"，不会让低秩条目越过高秩多名。0 = 停用该来源。
	RecallWeightReflection float64 `toml:"recall.weight_reflection"` // 1.2
	RecallWeightEpisodic   float64 `toml:"recall.weight_episodic"`   // 1.1
	RecallWeightSemantic   float64 `toml:"recall.weight_semantic"`   // 1.0
	RecallWeightRAG        float64 `toml:"recall.weight_rag"`        // 1.0
	// RecallRerankTopN / RecallRerankMinProb 校准相关度门（仅当注入了本地重排器时生效，ADR-0105 决策十）：
	// 对融合后前 N 条重排，丢弃相关概率 < MinProb 的条目。0.5 = 交叉编码器 yes/no 二分类的自然判定边界。
	// TopN=0 关闭该门。无重排器/重排失败/超时均跳过此门（不阻断召回）。
	RecallRerankTopN    int     `toml:"recall.rerank_top_n"`    // 12
	RecallRerankMinProb float64 `toml:"recall.rerank_min_prob"` // 0.5
	// RecallRAGMinSurprise SurpriseIndex 低于此值时本回合不查 RAG（环境平稳、无需外部知识补充）；
	// 高于 SurpriseHintThreshold 时 RAG 检索深度加倍。自原 Assembler 迁入回合内召回（ADR-0105 决策十）。
	RecallRAGMinSurprise float64 `toml:"recall.rag_min_surprise"` // 0.3

	// ── WP1 信任档位与快照阈值（ADR-0115）────────────────────────────────────────
	// CheckpointThrottleMaxPerTurn 同一回合内影子 git 写前快照的最大次数。
	// 超出后当回合剩余高风险操作仍执行，但跳过快照（避免 IO 风暴）。
	// 仅 full 档启用快照；standard/hardened 档此字段不生效。
	CheckpointThrottleMaxPerTurn int `toml:"checkpoint.throttle_max_per_turn"` // 20
	// CheckpointRetentionCount 保留的最近快照数（影子 git 保留的 commit 数）。
	// 超出时旧快照 gc 裁剪。0 = 不裁剪（不推荐用于长时间运行的 Agent）。
	CheckpointRetentionCount int `toml:"checkpoint.retention_count"` // 10
	// AgentsMaxConcurrent 并发子 Agent 上限（full 档 loop_mode 下的并发协程数上限）。
	// standard/hardened 档该字段备查，由上层 M4 调度器决定是否引用。
	AgentsMaxConcurrent int `toml:"agents_max_concurrent"` // 4
	// SecretGetMaxPerTurn full 档 secret_get 工具单回合最多读取凭据条数（防止批量泄露）。
	SecretGetMaxPerTurn int `toml:"secret_get.max_per_turn"` // 5

	// ── WP2 agentic 循环内核阈值（ADR-0115）──────────────────────────────────────
	// LoopMode 循环模式："agentic"（模型驱动自主循环）或 "staged"（原有五段式程序编排）。
	LoopMode string `toml:"loop_mode"` // "agentic"
	// LoopRunawayGuardRounds 失控保护轮数（<=0 表示使用档位默认值：full=0, standard=200, hardened=50）。
	LoopRunawayGuardRounds int `toml:"loop.runaway_guard_rounds"` // 200
	// LoopTurnBudgetUSD 单回合花费上限（USD），0 = 不限。
	LoopTurnBudgetUSD float64 `toml:"loop.turn_budget_usd"` // 0
	// LoopTurnBudgetTokens 单回合 Token 上限，0 = 不限。
	LoopTurnBudgetTokens int `toml:"loop.turn_budget_tokens"` // 0
	// LoopTurnWallSeconds 单回合物理执行时间上限（秒），0 = 不限。
	LoopTurnWallSeconds int `toml:"loop.turn_wall_seconds"` // 0
	// LoopObservationMaskAfterRounds 超过此轮数的历史工具结果将被批量遮蔽（T3 观察遮蔽）。
	LoopObservationMaskAfterRounds int `toml:"loop.observation_mask_after_rounds"` // 6
	// LoopObservationMaskBatch 观察遮蔽触发时单次批量驱逐的轮数。
	LoopObservationMaskBatch int `toml:"loop.observation_mask_batch"` // 4
	// LoopObservationMaskTriggerTokens 上下文估计达到该值才遮蔽（ADR-0120 决策四）；0=窗口的 50%，无窗口信息 64000。
	LoopObservationMaskTriggerTokens int `toml:"loop.observation_mask_trigger_tokens"` // 0
	LoopObservationMaskMinTokens     int `toml:"loop.observation_mask_min_tokens"`     // 1024：只遮蔽不少于该值的工具结果
	LoopRecallMaxChars               int `toml:"loop.recall_max_chars"`                // 2000：自动召回文本上限（字符），0=不限
	// LoopRepeatWarnAt 无进展检测：同一工具+同一规范化参数得到相同结果累计达到该次数时，在易变上下文末尾提醒一次；0=关闭检测。
	LoopRepeatWarnAt int `toml:"loop.repeat_warn_at"` // 3
	// LoopRepeatStopAt 无进展检测：达到该次数仍在原地打转则收尾（abort 并保存进度）；须大于 LoopRepeatWarnAt，0=只提醒不收尾。
	LoopRepeatStopAt int `toml:"loop.repeat_stop_at"` // 8
	// LoopCompletion 回合完成判定方式："auto"（无工具调用即完成）或 "explicit"（必须调用 finish 工具）。
	LoopCompletion string `toml:"loop.completion"` // "auto"
	// JobsKillOnSessionEnd 会话结束时是否清理该会话启动的后台进程（WP5，ADR-0115）。
	// full 档默认 false；standard/hardened 档默认 true。
	JobsKillOnSessionEnd bool `toml:"jobs.kill_on_session_end"`
	// SleepTimeIdleMinutes 会话空闲超过此时长（分钟）触发后台整理与失败挖掘（WP6，ADR-0115）。默认 10。
	SleepTimeIdleMinutes int `toml:"sleep_time.idle_minutes"`
	// SleepTimeEnabled 显式开关后台 sleep-time。若未显式配置，由当前 TrustProfile 策略决定。
	SleepTimeEnabled *bool `toml:"sleep_time.enabled"`
}

// Validate 校验 M4 阈值中需要解析的枚举字段，配置错误在加载时失败而非运行时静默回退。
func (t M4KernelThresholds) Validate() error {
	for key, v := range map[string]string{
		"thinking.perceive": t.ThinkingPerceive, "thinking.plan_initial": t.ThinkingPlanInitial,
		"thinking.reflect": t.ThinkingReflect, "thinking.respond": t.ThinkingRespond,
	} {
		if _, ok := types.ParseThinkingMode(v); !ok {
			return apperr.New(apperr.CodeInvalidInput, "m4_kernel."+key+": invalid thinking mode "+v+" (want disabled|low|high|max or empty)")
		}
	}
	for key, v := range map[string]string{
		"model_pool.perceive": t.ModelPoolPerceive, "model_pool.plan_initial": t.ModelPoolPlanInitial,
		"model_pool.plan_replan": t.ModelPoolPlanReplan, "model_pool.reflect": t.ModelPoolReflect,
		"model_pool.respond": t.ModelPoolRespond, "model_pool.validate": t.ModelPoolValidate,
		"model_pool.loop": t.ModelPoolLoop,
	} {
		switch types.ModelPool(v) {
		case types.ModelPoolDefault, types.ModelPoolGeneral, types.ModelPoolReasoning, types.ModelPoolBudget:
		default:
			return apperr.New(apperr.CodeInvalidInput, "m4_kernel."+key+": invalid model pool "+v+" (want default|general|reasoning|budget)")
		}
	}
	switch t.LoopMode {
	case "", "agentic", "staged":
	default:
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.loop_mode: invalid "+t.LoopMode+" (want agentic|staged)")
	}
	if t.LoopRepeatWarnAt < 0 || t.LoopRepeatStopAt < 0 {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.loop.repeat_*: must be >= 0")
	}
	if t.LoopRepeatStopAt > 0 && t.LoopRepeatStopAt <= t.LoopRepeatWarnAt {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.loop.repeat_stop_at: must be > repeat_warn_at (warn before stop)")
	}
	switch t.LoopCompletion {
	case "", "auto", "explicit":
	default:
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.loop.completion: invalid "+t.LoopCompletion+" (want auto|explicit)")
	}
	return t.validateRecall()
}

// validateRecall 召回预算阈值校验：非正的上限会让召回段恒为空或不设防，配置错误在加载时失败。
func (t M4KernelThresholds) validateRecall() error {
	if t.RecallItemMaxChars < 1 {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.recall.item_max_chars: must be >= 1")
	}
	if t.RecallMaxTokens < 1 {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.recall.max_tokens: must be >= 1")
	}
	if t.RecallMinScore < 0 {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.recall.min_score: must be >= 0")
	}
	if t.RecallMinScoreRatio < 0 || t.RecallMinScoreRatio > 1 {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.recall.min_score_ratio: must be within [0,1]")
	}
	for key, v := range map[string]float64{
		"weight_reflection": t.RecallWeightReflection, "weight_episodic": t.RecallWeightEpisodic,
		"weight_semantic": t.RecallWeightSemantic, "weight_rag": t.RecallWeightRAG,
	} {
		if v < 0 || math.IsNaN(v) {
			return apperr.New(apperr.CodeInvalidInput, "m4_kernel.recall."+key+": must be >= 0 (0 disables the source)")
		}
	}
	if t.RecallRerankTopN < 0 {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.recall.rerank_top_n: must be >= 0 (0 disables the gate)")
	}
	if t.RecallRerankMinProb < 0 || t.RecallRerankMinProb > 1 {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.recall.rerank_min_prob: must be within [0,1]")
	}
	if t.RecallRAGMinSurprise < 0 || t.RecallRAGMinSurprise > 1 {
		return apperr.New(apperr.CodeInvalidInput, "m4_kernel.recall.rag_min_surprise: must be within [0,1]")
	}
	return nil
}

func defaultLoopMode() string {
	if m := os.Getenv("POLARIS_LOOP_MODE"); m != "" {
		return m
	}
	// 在 go test 运行环境下且未显式指定环境变量时，默认保持 staged 保证全仓既有 staged 单测逐字节等价；
	// 生产环境下默认启用 agentic 驱动模型自主循环（ADR-0115 WP2）。
	for _, arg := range os.Args {
		if strings.HasSuffix(arg, ".test") || strings.Contains(arg, "-test.") {
			return "staged"
		}
	}
	if strings.HasSuffix(os.Args[0], ".test") {
		return "staged"
	}
	return "agentic"
}
