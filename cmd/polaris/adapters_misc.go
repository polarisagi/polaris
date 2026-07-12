package main

import (
	"github.com/polarisagi/polaris/internal/action"
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
