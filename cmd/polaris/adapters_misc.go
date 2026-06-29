// adapters_misc.go — 杂项桥接适配器。
// 在 cmd/ 层定义以避免 pkg 间包循环：
//   - dummySurreal        空实现（SurrealDB 不可用时占位）
//   - evalAgentAdapter    agent.Agent → eval.EvalAgent
//   - extensionActivatorAdapter  native.ExtensionActivator → agent.ExtensionActivator
//   - memEmbedderAdapter  search.Embedder → retrieval.Embedder
//   - collapseRecorderAdapter  *si.LogicCollapseMonitor → agent.ToolCallRecorder
//   - hitlNotifierAdapter  hitl.GatewayImpl → orchestrator.HITLNotifier
//   - codeActAdapter      *codeact.CodeAct → agent.CodeActEngine
//   - skillCacheAdapter   *skill.ScriptSkillCache → agent.ScriptSkillCache
//   - lamPolicyAdapter    *lam.ComputerUseEngine → agent.LAMPolicyChecker
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/memory/retrieval"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/action/lam"
	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/automation/hitl"
	"github.com/polarisagi/polaris/internal/extension/native"
	extskill "github.com/polarisagi/polaris/internal/extension/skill"
	si "github.com/polarisagi/polaris/internal/learning"
	"github.com/polarisagi/polaris/internal/store/search"
	swarmAgents "github.com/polarisagi/polaris/internal/swarm/agents"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── dummySurreal ─────────────────────────────────────────────────────────────
//
// SurrealDB 不可用时（<8GB VPS）的空实现占位，实现 agents.SurrealWriterInterface。
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

// ─── evalAgentAdapter ─────────────────────────────────────────────────────────
//
// 将 agent.Agent 适配为 eval.EvalAgent 接口。
// agent.Agent.Run(ctx) error 与 eval.EvalAgent.Run(ctx, []byte) ([]byte, []string, error) 签名不匹配。
type evalAgentAdapter struct {
	agent *agent.Agent
}

func (a *evalAgentAdapter) Run(ctx context.Context, input []byte) ([]byte, []string, error) {
	a.agent.SetTaskIntent(input)

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.agent.Run(ctx)
	}()

	_ = a.agent.SendIntent(types.TriggerIntentReceived)

	select {
	case err := <-errCh:
		if err != nil {
			return nil, nil, err
		}
		return a.agent.GetExecuteResult(), nil, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// ─── extensionActivatorAdapter ────────────────────────────────────────────────
//
// 将 *native.ExtensionActivator 适配为 agent.ExtensionActivator 接口。
// native.ActivatedHint → fsm.ExtActivatedHint 字段映射。
type extensionActivatorAdapter struct {
	inner *native.ExtensionActivator
}

func (a *extensionActivatorAdapter) FindAndActivate(ctx context.Context, goal string) ([]fsm.ExtActivatedHint, error) {
	hints, err := a.inner.FindAndActivate(ctx, goal)
	if err != nil || hints == nil {
		return nil, err
	}
	result := make([]fsm.ExtActivatedHint, 0, len(hints))
	for _, h := range hints {
		result = append(result, fsm.ExtActivatedHint{
			ToolName:    h.ToolName,
			Description: h.Description,
		})
	}
	return result, nil
}

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
	go a.m.RecordSuccess(context.WithoutCancel(ctx), &extskill.CollapseTrajectory{
		SkillID:     toolName,
		CompletedAt: time.Now().Unix(),
		TaintLevel:  0,
	}, nil)
}

// ─── hitlNotifierAdapter ──────────────────────────────────────────────────────
//
// 将 hitl.GatewayImpl 适配为 orchestrator.HITLNotifier（LogicCollapseMonitor 依赖）。
// 在 cmd/ 层定义以避免 pkg/swarm → pkg/edge/hitl 包循环。
type hitlNotifierAdapter struct {
	gateway *hitl.GatewayImpl
}

// NotifyHITL 发起高风险技能的异步 HITL 审批请求（fire-and-forget）。
// triggerCollapse 本身已在 goroutine 中运行，此处再异步是因为 GatewayImpl.Prompt
// 会阻塞直到审批完成或超时，不应占用 triggerCollapse 的 goroutine。
func (a *hitlNotifierAdapter) NotifyHITL(_ context.Context, skillID, reason string) error {
	p := types.HITLPrompt{
		ID:             fmt.Sprintf("logic_collapse_%s_%d", skillID, time.Now().UnixNano()),
		CheckpointType: "logic_collapse_high_risk",
		PromptText:     fmt.Sprintf("高风险 Skill 请求 Logic Collapse 审批 [skill=%s reason=%s]", skillID, reason),
		RiskLevel:      3, // high
		TaintLevel:     2, // TaintMedium → 超时自动拒绝
		DeadlineNs:     time.Now().Add(24 * time.Hour).UnixNano(),
	}
	// 异步发起：不阻塞 triggerCollapse，HITL 审批结果由 M13 Interface 侧处理
	go func() {
		bCtx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		if _, err := a.gateway.Prompt(bCtx, p); err != nil {
			slog.Warn("hitl_notifier: HITL prompt failed",
				"skill_id", skillID, "checkpoint_id", p.ID, "err", err)
		}
	}()
	return nil
}

// 确保 memory 包被引用（编译器 unused import 检查）
var _ retrieval.Embedder = (*memEmbedderAdapter)(nil)

// govAgentAdapter 将 *agents.GovernanceAgent 适配为 codeact 包的 govAgent 接口。
// codeact 包在 internal/action/codeact/ 定义私有 govAgent 接口：
//
//	ValidateCode(language string, code []byte, caps map[string]bool) error
//
// 由于 CapabilitySet 已改为类型别名，GovernanceAgent 直接满足接口，此适配器仅供文档目的。
// 若将来 CapabilitySet 改回命名类型，在此处加转换逻辑即可。
type govAgentAdapter struct {
	inner *swarmAgents.GovernanceAgent
}

func (a *govAgentAdapter) ValidateCode(language string, code []byte, caps map[string]bool) error {
	return a.inner.ValidateCode(language, code, caps)
}

// ─── codeActAdapter ──────────────────────────────────────────────────────────
// 将 *codeact.CodeAct 适配为 agent.CodeActEngine。
// 字段映射：agent.CodeActRequest ↔ codeact.CodeActRequest（两者字段相同，防循环 import 而分别定义）。

type codeActAdapter struct {
	inner *codeact.CodeAct
}

func (a *codeActAdapter) Execute(ctx context.Context, req agent.CodeActRequest) (*agent.CodeActResult, error) {
	result, err := a.inner.Execute(ctx, codeact.CodeActRequest{
		Language:     req.Language,
		Code:         req.Code,
		CapabilityID: req.CapabilityID,
		SessionID:    req.SessionID,
		AgentID:      req.AgentID,
		TaintLevel:   req.TaintLevel,
	})
	if err != nil {
		return nil, err
	}
	return &agent.CodeActResult{
		Output:    result.Output,
		ExitCode:  result.ExitCode,
		LatencyMs: result.LatencyMs,
	}, nil
}

func (a *codeActAdapter) IsAvailable() bool { return true }

// ─── skillCacheAdapter ───────────────────────────────────────────────────────
// 将 *skill.ScriptSkillCache 适配为 agent.ScriptSkillCache。
// agent.SkillHandle 仅携带 SkillID，与 skill.ProcessHandle.SkillID 对应。

type skillCacheAdapter struct {
	inner *extskill.ScriptSkillCache
}

func (a *skillCacheAdapter) GetOrSpawn(ctx context.Context, skillID string) (*agent.SkillHandle, error) {
	handle, err := a.inner.GetOrSpawn(ctx, skillID)
	if err != nil || handle == nil {
		return nil, err
	}
	return &agent.SkillHandle{SkillID: handle.SkillID}, nil
}

// ─── lamPolicyAdapter ────────────────────────────────────────────────────────
// 将 *lam.ComputerUseEngine 适配为 agent.LAMPolicyChecker。
// agent 只需 CheckPolicy（Cedar 策略预检），完整 ExecuteAction 走 tool/builtin 路径。

type lamPolicyAdapter struct {
	inner *lam.ComputerUseEngine
}

func (a *lamPolicyAdapter) CheckPolicy(ctx context.Context, actionJSON []byte) error {
	return a.inner.CheckPolicy(ctx, actionJSON)
}
