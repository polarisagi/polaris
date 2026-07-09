package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// PRM 语义打分融合通道。架构文档: docs/arch/M04-Agent-Kernel.md §4.5
// "Tier 1+ (启发式 + 1.5B 挂载 PRM 融合)"。
//
// 设计取舍（系统级复用，非另起炉灶）: 文档描述"加载极小 PRM"，但底层单槽位
// llama.cpp FFI（rust/substrate/src/llama_infer，P3-1）设计上同一时刻只常驻
// 一个模型（覆盖式替换），新增第二并发模型槽位属于更大的 FFI 协议改造，
// 不在本次范围。此处复用当前已挂载的 LocalProvider（无论是作为主策略模型
// 还是专门加载的小模型）与已有的 GBNF grammar 约束推理能力
// （LocalAdapter.Infer 早已支持 ResponseFormat.Type=="gbnf"），
// 对末步做三态离散打分，避免自由生成拖慢 100ms 预算。
// 若 Agent.provider 不是 LocalProvider（远程 API），PRM 融合不启用，
// 行为退化为 Tier0 纯静态路径，与旧行为完全一致。

// prmStepWeight PRM 语义打分在融合分数中的固定权重（§4.5 SSoT: 0.6）。
const prmStepWeight = 0.6

// prmTimeout 单次 PRM 打分硬超时；超时即安全降级纯静态分（§4.5: ">100ms 降级"）。
const prmTimeout = 100 * time.Millisecond

// prmGrammar 约束 PRM 输出必须是三个离散 token 之一。
const prmGrammar = `root ::= "+1" | "0" | "-1"`

// prmSummaryMaxLen 步骤摘要截断长度，避免超长 prompt 侵占 100ms 预算。
const prmSummaryMaxLen = 200

// newStepScorer 构造 Adaptive Max-Steps 打分器。Tier1+ 且 provider 实现
// protocol.LocalProvider 时挂载 PRM 语义打分融合通道；否则退化为
// newDefaultStepScorer() 的纯静态行为（Tier0 或远程 Provider）。
func newStepScorer(provider protocol.Provider) *stepScorer {
	s := newDefaultStepScorer()
	lp, ok := provider.(protocol.LocalProvider)
	if !ok {
		return s
	}
	fg := probe.GlobalFeatureGate()
	if fg == nil || fg.HardwareTier() < probe.Tier1 {
		return s
	}
	s.prm = lp
	return s
}

// scoreWithPRM 计算静态分，PRM 可用时按 prmStepWeight 融合语义打分。
// PRM 超时/推理错误/OOM（FFI panic 经 catch_unwind 转换为 Go error）均安全
// 降级为纯静态分，不向上抛出 error——步骤打分是尽力而为的优化信号，不应
// 阻断/污染主执行路径。
func (s *stepScorer) scoreWithPRM(ctx context.Context, c stepCtx, stepSummary string) float64 {
	staticScore := s.score(c)
	if s.prm == nil {
		return staticScore
	}
	prmScore, ok := s.runPRM(ctx, stepSummary)
	if !ok {
		return staticScore
	}
	fused := (1-prmStepWeight)*staticScore + prmStepWeight*prmScore
	switch {
	case fused < 0:
		fused = 0
	case fused > 1:
		fused = 1
	}
	return fused
}

// runPRM 调用挂载的 LocalProvider 做 GBNF grammar 约束的三态语义打分
// (+1/0/-1 → 1.0/0.5/0.0)，100ms 硬超时（独立于调用方 ctx 的截止时间，取更严格者）。
func (s *stepScorer) runPRM(ctx context.Context, stepSummary string) (float64, bool) {
	cctx, cancel := context.WithTimeout(ctx, prmTimeout)
	defer cancel()

	msgs := []types.Message{
		{Role: "system", Content: "你是步骤质量评分器。只输出 +1、0 或 -1 三者之一，不要任何其它文字。"},
		{Role: "user", Content: stepSummary},
	}
	//custom-nolint:bare-infer // 历史代码暂留，后续重构替换
	resp, err := s.prm.Infer(cctx, msgs,
		types.WithMaxTokens(4),
		types.WithResponseFormat(&types.ResponseFormat{Type: "gbnf", Grammar: prmGrammar}),
	)
	if err != nil {
		slog.Debug("step_scorer: PRM scoring degraded to static (timeout/error/OOM)", "err", err)
		return 0, false
	}
	switch strings.TrimSpace(resp.Content) {
	case "+1":
		return 1.0, true
	case "0":
		return 0.5, true
	case "-1":
		return 0.0, true
	default:
		slog.Debug("step_scorer: PRM returned non-conforming output, degraded to static",
			"content", resp.Content)
		return 0, false
	}
}

// summarizeStepForPRM 构造供 PRM 打分的简短步骤摘要（截断，避免拖慢 100ms 预算）。
func summarizeStepForPRM(toolName string, ok bool, res *types.ToolResult, err error) string {
	status := "success"
	if !ok {
		status = "failed"
	}
	detail := ""
	switch {
	case err != nil:
		detail = err.Error()
	case res != nil && res.Error != "":
		detail = res.Error
	case res != nil:
		detail = string(res.Output)
	}
	if len(detail) > prmSummaryMaxLen {
		detail = detail[:prmSummaryMaxLen]
	}
	return fmt.Sprintf("tool=%s status=%s detail=%s", toolName, status, detail)
}
