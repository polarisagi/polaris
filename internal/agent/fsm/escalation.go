package fsm

// FailureKind 回合内失败/未达成的成因分类（ADR-0101 决策六）。
//
// 升级到更贵的模型只对"模型能力不足"类失败有效：安全闸门拒绝换 Pro 照样被拒，
// 网络超时换 Pro 照样超时，观察—再规划是任务的正常推进而非失败。旧规则把一切
// 重规划都升级到 reasoning + ThinkingMax，这些成因的重规划全部多付了钱。
type FailureKind int

const (
	// FailurePolicy 安全/策略拒绝（L1_taint / L1_policy / L2_heuristic / L3_llm）：不升级。
	FailurePolicy FailureKind = iota
	// FailureTransient 瞬时故障（超时/网络/限流/Provider 耗尽/TOCTOU 冲突）：不升级。
	FailureTransient
	// FailureGoalUnmet 反思判定未达成、进入观察—再规划：任务推进，不升级。
	FailureGoalUnmet
	// FailureToolError 工具报错（参数错、找不到对象等）：首次靠反馈自纠，重复出现才升级。
	FailureToolError
	// FailurePlanInvalid 计划结构/格式不可用（L0 拓扑、无法解析）：能力不足，升级一级。
	FailurePlanInvalid
	// FailureSelfEscalate 规划模型自评超出能力（plan 输出 escalate=true）：直接升到 reasoning。
	FailureSelfEscalate
)

// maxEscalation 升级阶梯顶端（见 metrics.SelectPlanTier：0 general/无思考 … 3 reasoning/Max）。
const maxEscalation = 3

// RecordFailure 按成因累计本回合的升级级数。每回合新建 Agent（见 RespondAttempts 注释），
// 计数天然按回合隔离。
func (s *StateContext) RecordFailure(kind FailureKind) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	switch kind {
	case FailurePlanInvalid:
		s.Escalation++
	case FailureToolError:
		s.ToolErrorCount++
		if s.ToolErrorCount >= 2 {
			s.Escalation++
		}
	case FailureSelfEscalate:
		if s.Escalation < 2 {
			s.Escalation = 2
		}
	case FailurePolicy, FailureTransient, FailureGoalUnmet:
		// 换更贵的模型不改变结果，只靠 ReplanFeedback 让模型换方案。
	}
	if s.Escalation > maxEscalation {
		s.Escalation = maxEscalation
	}
}

// ClassifyValidationLayer 把 S_VALIDATE 拒绝层映射为失败成因。只有 L0（拓扑/结构）
// 反映规划能力；其余各层是安全/策略判定，升级模型不会让被禁止的动作变得被允许。
func ClassifyValidationLayer(layer string) FailureKind {
	if layer == "L0" {
		return FailurePlanInvalid
	}
	return FailurePolicy
}
