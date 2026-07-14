package curriculum

import (
	"context"
)

// m9_export.go — 为外部测试包导出内部 API（仅测试辅助）。
//
// 2026-07-14（ADR-0051）：NewDifficultyCalibrator/AddSample/Thresholds 随
// calibrator.go（DynamicDifficultyCalibrator 等）一并删除，理由见 m9_test.go
// 顶部注释。

// SafetyAuditPublic 对外暴露 passSafetyAudit（测试辅助）。
func (ag *AutoCurriculumGenerator) SafetyAuditPublic(ctx context.Context, sample *CurriculumSample) bool {
	return ag.passSafetyAudit(ctx, sample)
}

// IsFrozenPublic 对外暴露 isFrozen（测试辅助）。
func (ag *AutoCurriculumGenerator) IsFrozenPublic(skill string) bool {
	return ag.isFrozen(skill)
}
