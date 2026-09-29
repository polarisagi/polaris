package types

// LLM 调用用途常量（ADR-0105 决策五）。
//
// 为什么集中定义：purpose 是 llm_calls 记账归因、精确响应缓存白名单（决策六）与错峰调度
// （决策七）三处共同的键。字面量散落在调用点时，拼写漂移会让某一处悄悄失效（记账记成
// unspecified、白名单对不上、缓存永不命中），而三者都没有编译期报错。
//
// 取值一律 snake_case，与既有 llm_calls.purpose 历史数据保持同值——本文件只归拢，
// 不改任何已有字符串的值。新增调用点必须先在此登记常量，再由
// tools/llm_call_opts_lint.go 保证调用时同时声明 WithPurpose 与 WithThinkingMode。
const (
	// ── 内核阶段（FSM 驱动，依赖会话状态与安全门；禁止任何响应缓存，见 IsKernelPurpose）──
	PurposePerceive         = "perceive"
	PurposePlan             = "plan"
	PurposeExecute          = "execute"
	PurposeReflect          = "reflect"
	PurposeRespond          = "respond"
	PurposeValidateWatchdog = "validate_watchdog"
	PurposeKernel           = "kernel" // llmPurposeOf 的兜底值：未识别的 FSM 状态
	PurposePlanPRMCandidate = "plan_prm_candidate"

	// ── 记忆 / 摘要 ──
	PurposeCompactSummary     = "compact_summary"
	PurposeConsolidateSummary = "consolidate_summary"
	PurposeDurativeMemory     = "durative_memory"
	PurposeMemoryWriteFilter  = "memory_write_filter"
	PurposePersonaRefine      = "persona_refine"
	PurposeReflexion          = "reflexion"

	// ── 知识 / RAG ──
	PurposeGraphRAGExtract   = "graphrag_extract"
	PurposeGraphRAGSummary   = "graphrag_summary"
	PurposeGraphRAGCommunity = "graphrag_community"
	PurposeGraphRAGConcept   = "graphrag_concept"
	PurposeRAGSummaryTree    = "rag_summary_tree"
	PurposeRAGQueryRewrite   = "rag_query_rewrite"

	// ── 规划 / 执行辅助 ──
	PurposePlannerPatchCandidate = "planner_patch_candidate"
	PurposePlannerPlanCandidate  = "planner_plan_candidate"
	PurposeTaskDecompose         = "task_decompose"
	PurposePlanPRMScore          = "plan_prm_score"
	PurposeStepPRMScore          = "step_prm_score"
	PurposeLAMResolveAction      = "lam_resolve_action"

	// ── 扩展 ──
	PurposeSkillCreate  = "skill_create"
	PurposePluginCreate = "plugin_create"
	PurposeMCPSampling  = "mcp_sampling"
	PurposeHookPrompt   = "hook_prompt"

	// ── 安全 / 判官 ──
	PurposeFactualityJudge = "factuality_judge"

	// ── 学习 / 自进化 ──
	PurposePromptOptimizerGradient    = "prompt_optimizer_gradient"
	PurposePromptOptimizerContrastive = "prompt_optimizer_contrastive"
	PurposeCurriculumGenerate         = "curriculum_generate"
	PurposeCurriculumSICDetect        = "curriculum_sic_detect"
	PurposeCurriculumSafetyJudge      = "curriculum_safety_judge"
	PurposeLogicCollapseCodegen       = "logic_collapse_codegen"
	PurposeSyntheticEvalGen           = "synthetic_eval_gen"
	PurposeSyntheticSkillGen          = "synthetic_skill_gen"

	// ── 评测 ──
	PurposeShadowCandidate = "shadow_candidate"
	PurposeShadowJudge     = "shadow_judge"
	PurposeSamplingScore   = "sampling_score"
	PurposeEvalL4Judge     = "eval_l4_judge"

	// ── 其他 ──
	PurposeBackgroundLLMInfer = "background_llm_infer" // boot_tools llmInfer 的兜底用途，调用方可覆盖
	PurposeUnspecified        = "unspecified"          // llm_calls 对空 purpose 的记账值
)

// IsKernelPurpose 报告 purpose 是否属于内核阶段（含 unspecified）。
//
// 这是精确响应缓存的**硬排除**判据（ADR-0105 决策六）：内核调用依赖会话状态、工具结果与
// 安全门，跨会话复用旧回答可能泄漏或给出过期结论；unspecified 无从判断性质，同样拒绝。
// 用 switch 而非包级 map：本包禁止可变全局变量（ADR-0001）。
func IsKernelPurpose(purpose string) bool {
	switch purpose {
	case PurposePerceive, PurposePlan, PurposeExecute, PurposeReflect, PurposeRespond,
		PurposeValidateWatchdog, PurposeKernel, PurposePlanPRMCandidate,
		PurposeUnspecified, "", "validate":
		return true
	}
	return false
}
