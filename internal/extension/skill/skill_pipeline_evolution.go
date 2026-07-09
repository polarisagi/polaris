package skill

// ============================================================================
// SkillEvolutionEngine — 递归演化 + 四级废弃（R7 拆分自 skill_pipeline.go，
// 四步验证管线 SkillValidationPipeline 见 skill_pipeline.go）。
// 注意：本文件与 skill_evolution.go 的 SkillEvolutionMonitor 是两套独立机制——
// Monitor 周期扫描 episodic_events 触发 Logic Collapse 重编译；Engine 是内存态
// 逐次调用累积成功/失败历史后驱动四级废弃判定，二者不可合并（数据源与触发时机不同）。
// 架构文档: docs/arch/06-Skill-Library-深度选型.md §4
// ============================================================================

type SkillEvolutionEngine struct {
	skills           map[string]*Skill
	successHistories map[string][]bool
	failureReasons   map[string][]string
}

// EvaluateAndEvolve 评估并演化。
// 步骤1: UncontrollableFailure (网络不可达/API配额/OS kill) → 跳过
// 步骤2: 追加 result.Success 到 SuccessHistory
// 步骤3: 连续失败 >= 3 → 按策略分发:
//
//	UpdateValidate → Revalidate 重新测试; UpdateReflect → LLM反思改进; UpdateDiscard → deprecated
//
// 步骤4: SuccessHistory 保留最近 20 条
// 步骤5: 连续 UncontrollableFailure > 100 → 冻结废弃评估, 每60s探测
func (e *SkillEvolutionEngine) EvaluateAndEvolve(skillID string, success bool, reason string) {
	history := e.successHistories[skillID]
	history = append(history, success)
	if len(history) > 20 {
		history = history[len(history)-20:]
	}
	e.successHistories[skillID] = history

	// 连续 3 次 ControllableFailure → 触发演化
	if consecutiveFailures(history) >= 3 {
		e.triggerEvolution(skillID)
	}
}

func consecutiveFailures(history []bool) int {
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		if !history[i] {
			count++
		} else {
			break
		}
	}
	return count
}

// triggerEvolution 根据技能状态分发演化策略。
// - 成功率 < 30% 且使用次数 > 10 → DeprecationDynamic（移出主索引，保留手动恢复）
// - 连续失败但成功率尚可 → 标记待 LLM 反思改进
// - 安全漏洞/签名失效 → DeprecationHard（物理删除脚本 + 撤销签名）
func (e *SkillEvolutionEngine) triggerEvolution(skillID string) {
	sk, ok := e.skills[skillID]
	if !ok {
		return
	}

	history := e.successHistories[skillID]
	if len(history) == 0 {
		return
	}

	// 计算近期成功率
	recent := history
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}
	successes := 0
	for _, s := range recent {
		if s {
			successes++
		}
	}
	successRate := float64(successes) / float64(len(recent))

	// 四级废弃判定
	switch {
	case successRate < 0.3 && sk.UseCount > 10:
		// 动态废弃: 移出主索引，保留手动恢复路径
		sk.Deprecated = true
		sk.DeprecationLevel = int(DeprecationDynamic)
	case len(consecutiveFailureReasons(e.failureReasons[skillID])) >= 5:
		// 连续 5 次不可控失败 → 暂停使用，每 60s 探测
		sk.Deprecated = true
		sk.DeprecationLevel = int(DeprecationFiltered)
	default:
		// 标记待 LLM 反思改进（下次 Revalidate 触发）
		sk.NeedsRevalidate = true
	}
}

func consecutiveFailureReasons(reasons []string) []string {
	var result []string
	for i := len(reasons) - 1; i >= 0; i-- {
		if reasons[i] != "" {
			result = append(result, reasons[i])
		} else {
			break
		}
	}
	return result
}

// SkillDeprecationLevel 四级废弃。
type SkillDeprecationLevel int

const (
	DeprecationNormal   SkillDeprecationLevel = iota // LLM 生成更好版本 → version++
	DeprecationFiltered                              // 连续 3 次测试失败 → deprecated=true
	DeprecationDynamic                               // 成功率 < 30% 且使用 > 10 → 移出主索引
	DeprecationHard                                  // 安全漏洞/签名失效 → 物理删除脚本 + 撤销签名
)

var (
	ErrPipelineTaintedTrajectory = &SkillPipelineError{"tainted trajectory rejected"}
	ErrSkillCompileFailed        = &SkillPipelineError{"skill compilation failed"}
)

type SkillPipelineError struct{ msg string }

func (e *SkillPipelineError) Error() string { return e.msg }
