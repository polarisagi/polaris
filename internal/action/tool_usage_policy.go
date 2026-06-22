package action

import (
	"fmt"
	"sync"
) // ToolUsagePolicy 描述工具的最优参数建议和适用场景。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §8.2
type ToolUsagePolicy struct {
	ToolName          string
	ParamHints        map[string]ParamHint // 最优参数建议
	BestFor           []string             // 适用场景
	NotRecommendedFor []string             // 不适用场景
}

// ParamHint 提供参数级别的最优值建议。
type ParamHint struct {
	DefaultValue any    `json:"default_value"`
	Description  string `json:"description"`
	MinValue     any    `json:"min_value,omitempty"`
	MaxValue     any    `json:"max_value,omitempty"`
}

// ToolOutcome 一次工具调用的结果反馈，供 PolicyEvolver 学习。
type ToolOutcome struct {
	ToolName  string
	Success   bool
	LatencyMs int64
	Error     string
	Params    map[string]any // 本次使用的参数
}

// FailurePattern 失败模式签名
type FailurePattern struct {
	ErrorType  string
	InputFeat  string
	Frequency  int
	Mitigation string
}

// PolicyEvolver 基于工具调用历史动态演化 ToolUsagePolicy。
//
// 当前实现：滑动窗口（最近 N 次）统计成功率，成功率低于阈值时更新
// NotRecommendedFor 场景标注，供上层推理层注入 Prompt 参数提示。
//
// M4 DAG 注入路径：调用方在 InferRequest 构建时通过 PolicyEvolver.GetContextHint()
// 读取最新策略，并注入 System Prompt 的 <tool-hints> 块。
//
// [待完整实现：当前为无监督统计版本；Tier-1+ 计划接入 Eval Harness 反馈信号]
type PolicyEvolver struct {
	mu       sync.RWMutex
	policies map[string]*ToolUsagePolicy
	history  map[string][]ToolOutcome
	patterns map[string]map[string]*FailurePattern // toolName -> patternKey -> pattern
	window   int                                   // 统计窗口大小（默认 50）
	minRate  float64                               // 成功率下限（低于此值标注为 "不推荐"，默认 0.6）
}

// NewPolicyEvolver 创建策略演化器。
func NewPolicyEvolver(window int, minSuccessRate float64) *PolicyEvolver {
	if window <= 0 {
		window = 50
	}
	if minSuccessRate <= 0 {
		minSuccessRate = 0.6
	}
	return &PolicyEvolver{
		policies: make(map[string]*ToolUsagePolicy),
		history:  make(map[string][]ToolOutcome),
		patterns: make(map[string]map[string]*FailurePattern),
		window:   window,
		minRate:  minSuccessRate,
	}
}

// RegisterPolicy 注册或替换工具的初始策略。
func (e *PolicyEvolver) RegisterPolicy(policy *ToolUsagePolicy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.policies[policy.ToolName] = policy
}

// GetPolicy 返回工具的当前策略（不存在时返回 nil）。
func (e *PolicyEvolver) GetPolicy(toolName string) *ToolUsagePolicy {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.policies[toolName]
}

// ListPolicies 返回所有已注册工具的策略快照。
func (e *PolicyEvolver) ListPolicies() []*ToolUsagePolicy {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*ToolUsagePolicy, 0, len(e.policies))
	for _, p := range e.policies {
		out = append(out, p)
	}
	return out
}

// RecordOutcome 记录一次工具调用结果，并触发策略更新。
func (e *PolicyEvolver) RecordOutcome(outcome ToolOutcome) {
	e.mu.Lock()
	defer e.mu.Unlock()

	hist := e.history[outcome.ToolName]
	hist = append(hist, outcome)
	// 保持窗口大小：丢弃最旧的
	if len(hist) > e.window {
		hist = hist[len(hist)-e.window:]
	}
	e.history[outcome.ToolName] = hist

	e.evolvePolicy(outcome.ToolName, hist)
}

// evolvePolicy 根据历史计算成功率，更新策略的 NotRecommendedFor 场景标注。
// 必须在持锁状态下调用。
func (e *PolicyEvolver) evolvePolicy(toolName string, hist []ToolOutcome) {
	if len(hist) < 5 {
		// 样本不足，不干预策略
		return
	}

	successCount := 0
	var totalLatencyMs int64
	for _, o := range hist {
		if o.Success {
			successCount++
		}
		totalLatencyMs += o.LatencyMs
	}
	rate := float64(successCount) / float64(len(hist))
	avgLatencyMs := totalLatencyMs / int64(len(hist))

	policy, ok := e.policies[toolName]
	if !ok {
		// 策略不存在，自动创建基础策略
		policy = &ToolUsagePolicy{
			ToolName:   toolName,
			ParamHints: make(map[string]ParamHint),
		}
		e.policies[toolName] = policy
	}

	// 成功率过低 → 标注"高失败率"场景
	flagLowRate := "high_failure_rate"
	if rate < e.minRate {
		if !containsStr(policy.NotRecommendedFor, flagLowRate) {
			policy.NotRecommendedFor = append(policy.NotRecommendedFor, flagLowRate)
		}
	} else {
		policy.NotRecommendedFor = removeStr(policy.NotRecommendedFor, flagLowRate)
	}

	// 高延迟 → 更新 ParamHint timeout 建议
	if avgLatencyMs > 5000 {
		policy.ParamHints["timeout_ms"] = ParamHint{
			DefaultValue: avgLatencyMs * 2,
			Description:  "自动推断：基于近期平均延迟的超时建议",
		}
	}

	// 记录失败模式
	lastOutcome := hist[len(hist)-1]
	if !lastOutcome.Success && lastOutcome.Error != "" {
		e.extractFailurePattern(toolName, lastOutcome)
	}
}

func (e *PolicyEvolver) extractFailurePattern(toolName string, outcome ToolOutcome) {
	// 简单解析 Error 类型
	errType := "Unknown"
	if len(outcome.Error) > 0 {
		errType = outcome.Error // 简单地将错误信息本身作为 ErrorType（实际应通过正则/关键字分析）
	}

	// 生成签名
	patternKey := errType // 此处可更精细：结合 params 等

	if e.patterns[toolName] == nil {
		e.patterns[toolName] = make(map[string]*FailurePattern)
	}

	pattern := e.patterns[toolName][patternKey]
	if pattern == nil {
		pattern = &FailurePattern{
			ErrorType: errType,
			Frequency: 0,
		}
		e.patterns[toolName][patternKey] = pattern
	}
	pattern.Frequency++

	// Frequency >= 3 -> 暂无 LLM 生成，直接提示用户或通过规则进行缓解
	if pattern.Frequency >= 3 && pattern.Mitigation == "" {
		pattern.Mitigation = "Consider breaking down the input or checking argument formats."
	}
}

// GetContextHint 返回工具的自适应使用提示字符串（供调用方注入 Prompt）。
// 仅当历史调用 ≥ 20 次时激活（冷启动不注入噪声）。
func (e *PolicyEvolver) GetContextHint(toolName string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	hist := e.history[toolName]
	if len(hist) < 20 {
		return ""
	}

	policy, ok := e.policies[toolName]
	if !ok {
		return ""
	}

	var hint string
	if len(policy.ParamHints) > 0 {
		hint += "ParamHints: "
		for k, v := range policy.ParamHints {
			hint += k + " (default: " + fmt.Sprintf("%v", v.DefaultValue) + " - " + v.Description + "); "
		}
	}

	// FailurePattern 告警
	if pats, exists := e.patterns[toolName]; exists {
		for _, p := range pats {
			if float64(p.Frequency) > 0.3*float64(len(hist)) {
				hint += "FailureWarning: Frequent error '" + p.ErrorType + "'. "
				if p.Mitigation != "" {
					hint += "Mitigation: " + p.Mitigation + "; "
				}
			}
		}
	}

	return hint
}

// BuildSystemHintBlock 将所有注册工具的提示聚合为标准 <tool-hints> XML 块，
// 供 M4 DAG InferRequest 构建时注入 System Prompt 的 ZoneMutableSkill 区。
//
// 格式示例：
//
//	<tool-hints>
//	  <tool name="browser_use">FailureWarning: Frequent timeout. Mitigation: reduce scope; </tool>
//	  <tool name="code_exec">ParamHints: timeout_ms (default: 10000 - ...); </tool>
//	</tool-hints>
//
// 无任何提示时返回空字符串（调用方不注入）。
func (e *PolicyEvolver) BuildSystemHintBlock() string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var body string
	for toolName := range e.policies {
		hist := e.history[toolName]
		if len(hist) < 20 {
			continue
		}
		hint := e.buildHintLocked(toolName, hist)
		if hint == "" {
			continue
		}
		body += "  <tool name=\"" + toolName + "\">" + hint + "</tool>\n"
	}
	if body == "" {
		return ""
	}
	return "<tool-hints>\n" + body + "</tool-hints>"
}

// buildHintLocked 内部辅助（调用方持有读锁）。
func (e *PolicyEvolver) buildHintLocked(toolName string, hist []ToolOutcome) string {
	policy, ok := e.policies[toolName]
	if !ok {
		return ""
	}
	var hint string
	if len(policy.ParamHints) > 0 {
		hint += "ParamHints: "
		for k, v := range policy.ParamHints {
			hint += k + " (default: " + fmt.Sprintf("%v", v.DefaultValue) + " - " + v.Description + "); "
		}
	}
	if pats, exists := e.patterns[toolName]; exists {
		for _, p := range pats {
			if float64(p.Frequency) > 0.3*float64(len(hist)) {
				hint += "FailureWarning: Frequent error '" + p.ErrorType + "'. "
				if p.Mitigation != "" {
					hint += "Mitigation: " + p.Mitigation + "; "
				}
			}
		}
	}
	return hint
}

// SuccessRate 返回指定工具在当前窗口内的成功率（0-1），无历史时返回 -1。
func (e *PolicyEvolver) SuccessRate(toolName string) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	hist := e.history[toolName]
	if len(hist) == 0 {
		return -1
	}
	success := 0
	for _, o := range hist {
		if o.Success {
			success++
		}
	}
	return float64(success) / float64(len(hist))
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func removeStr(ss []string, s string) []string {
	out := ss[:0]
	for _, v := range ss {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
