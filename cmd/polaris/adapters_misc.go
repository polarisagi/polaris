package main

import (
	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── policyEvolverOutcomeAdapter ────────────────────────────────────────────
//
// 桥接 polartool.ToolOutcomeRecorder（consumer-side 接口，定义在
// internal/tool，防止其反向依赖 internal/action）→ action.PolicyEvolver.
// RecordOutcome（2026-07-12 unwired-code-audit 补齐工具自进化闭环写侧）。
type policyEvolverOutcomeAdapter struct {
	pe *action.PolicyEvolver
}

func (a *policyEvolverOutcomeAdapter) RecordToolOutcome(toolName string, success bool, latencyMs int64, errMsg string) {
	a.pe.RecordOutcome(action.ToolOutcome{
		ToolName:  toolName,
		Success:   success,
		LatencyMs: latencyMs,
		Error:     errMsg,
	})
}

// ─── skillStaticAnalyzerAdapter ─────────────────────────────────────────────
//
// 桥接 lifecycle.ScriptStaticAnalyzer（consumer-side 接口，定义在
// internal/extension/lifecycle，防止其反向依赖 internal/extension/skill——
// skill 包经 skill_creator.go 已导入 internal/extension/marketplace，而
// marketplace 又导入 lifecycle，直接导入会形成循环）→
// skill.StaticAnalyzer.Analyze（返回形状不同，需要适配；skill.RiskAssessor.Assess
// 签名恰好与 lifecycle.ScriptRiskAssessor 一致，可直接传入，无需适配器）。
type skillStaticAnalyzerAdapter struct {
	inner *skill.StaticAnalyzer
}

func (a *skillStaticAnalyzerAdapter) Analyze(code []byte) (bool, []string, error) {
	ar, err := a.inner.Analyze(code)
	if err != nil {
		return false, nil, apperr.Wrap(apperr.CodeInternal, "skillStaticAnalyzerAdapter.Analyze", err)
	}
	return ar.Passed, ar.Violations, nil
}

// ─── dummySurreal ─────────────────────────────────────────────────────────────
//
// SurrealDB 不可用时（<8GB VPS）的空实现占位，实现 connector.SurrealWriterInterface。
type dummySurreal struct{}

func (d dummySurreal) FTSIndex(docID, text string) error {
	return apperr.New(apperr.CodeInternal, "SurrealDB not available")
}
func (d dummySurreal) VecUpsert(id string, embedding []float32) error {
	return apperr.New(apperr.CodeInternal, "SurrealDB not available")
}
func (d dummySurreal) GraphRelate(fromID, edgeType, toID string, weight float64) error {
	return apperr.New(apperr.CodeInternal, "SurrealDB not available")
}
func (d dummySurreal) FTSDelete(docID string) error {
	return apperr.New(apperr.CodeInternal, "SurrealDB not available")
}
func (d dummySurreal) VecDelete(id string) error {
	return apperr.New(apperr.CodeInternal, "SurrealDB not available")
}
func (d dummySurreal) VecKNN(query []float32, k int) ([]types.CognitiveSearchResult, error) {
	return nil, apperr.New(apperr.CodeInternal, "SurrealDB not available")
}
func (d dummySurreal) FTSSearch(query string, k int) ([]types.CognitiveSearchResult, error) {
	return nil, apperr.New(apperr.CodeInternal, "SurrealDB not available")
}
