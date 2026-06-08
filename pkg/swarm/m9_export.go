package swarm

import "context"

// m9_export.go — 为外部测试包导出内部 API（仅测试辅助）。

// NewDifficultyCalibrator 创建 DynamicDifficultyCalibrator。
func NewDifficultyCalibrator(targetRate, adjustStep float64) *DynamicDifficultyCalibrator {
	return &DynamicDifficultyCalibrator{
		targetSuccessRate: targetRate,
		adjustStep:        adjustStep,
		currentLow:        0.3,
		currentHigh:       0.6,
	}
}

// AddSample 追加难度样本（测试辅助）。
func (ddc *DynamicDifficultyCalibrator) AddSample(s DifficultySample) {
	ddc.history = append(ddc.history, s)
}

// Thresholds 返回当前低/高阈值（测试辅助）。
func (ddc *DynamicDifficultyCalibrator) Thresholds() (low, high float64) {
	return ddc.currentLow, ddc.currentHigh
}

// SafetyAuditPublic 对外暴露 passSafetyAudit（测试辅助）。
func (ag *AutoCurriculumGenerator) SafetyAuditPublic(ctx context.Context, sample *CurriculumSample) bool {
	return ag.passSafetyAudit(ctx, sample)
}

// IsFrozenPublic 对外暴露 isFrozen（测试辅助）。
func (ag *AutoCurriculumGenerator) IsFrozenPublic(skill string) bool {
	return ag.isFrozen(skill)
}

// NewPromptOptimizerMVP 创建最简 PromptOptimizer（无外部依赖，用于单元测试）。
// provider=nil 走规则 fallback；versionStore=nil 跳过 DB 持久化。
func NewPromptOptimizerMVP() *PromptOptimizer {
	return NewPromptOptimizer(nil, nil, 0)
}
