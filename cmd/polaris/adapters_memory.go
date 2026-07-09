package main

import (
	"context"
	"time"

	extskill "github.com/polarisagi/polaris/internal/extension/skill"
	si "github.com/polarisagi/polaris/internal/learning"
	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── memEmbedderAdapter ───────────────────────────────────────────────────────
//
// 将 search.Embedder 适配为 retrieval.Embedder。
// search.Embedder 接口: Embed(text string) []float32（无 ctx，无 ModelVersion）。
// retrieval.Embedder 接口: Embed(ctx, text) ([]float32, error) + ModelVersion() string。
// 此适配器仅用于 OnlineReindexer 注入路径（cmd/polaris/main.go §4.10.5）。
type memEmbedderAdapter struct {
	e     search.Embedder
	model string
}

func (a *memEmbedderAdapter) Embed(_ context.Context, text string) ([]float32, error) {
	v := a.e.Embed(text)
	if len(v) == 0 {
		// search.Embedder 无 error 返回；nil 向量唯一语义是 Embedder 暂不可用（如 Ollama 未启动）。
		// 转换为 error 让 OnlineReindexer 可区分失败与正常空结果，避免写入零向量污染索引。
		return nil, apperr.New(apperr.CodeInternal, "embedder returned empty vector")
	}
	return v, nil
}

func (a *memEmbedderAdapter) ModelVersion() string { return a.model }

// ─── collapseRecorderAdapter ──────────────────────────────────────────────────
//
// 将 *si.LogicCollapseMonitor 适配为 agent.ToolCallRecorder。
// 每次工具调用成功时以 toolName 作 SkillID 累积轨迹；
// 同一工具 ≥ 阈值次成功 → LogicCollapseMonitor 异步触发 Skill 蒸馏（M9 §4）。
type collapseRecorderAdapter struct{ m *si.LogicCollapseMonitor }

func (a *collapseRecorderAdapter) RecordToolSuccess(ctx context.Context, toolName string) {
	//custom-nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
	go a.m.RecordSuccess(context.WithoutCancel(ctx), &extskill.CollapseTrajectory{
		SkillID:     toolName,
		CompletedAt: time.Now().Unix(),
		TaintLevel:  0,
	}, nil)
}

// 确保 memory 包被引用（编译器 unused import 检查）
var _ retrieval.Embedder = (*memEmbedderAdapter)(nil)
