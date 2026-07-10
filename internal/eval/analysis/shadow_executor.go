package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"

	"go.opentelemetry.io/otel"
)

// ShadowExecutor 负责基于 EventLog 异步回放的轻量影子执行。
type ShadowExecutor struct {
	db          protocol.SQLQuerier
	llmProvider protocol.Provider
	mockCache   repo.MockResponseCache
	evalStore   *harness.SQLiteEvalStore
	staging     optimizer.StagingPipeline
	thresholds  config.Thresholds

	// 回放游标：进程内保证批次间不重复回放（offset 严格递增消费）。
	// 有意不持久化：重启后从 0 重扫的代价被采样率与 mock 缓存约束，
	// 换取不新增 DDL 表、不跨模块耦合 KV 存储（Tier-0 取舍）。
	mu         sync.Mutex
	lastOffset int64
}

// NewShadowExecutor 构造 ShadowExecutor。
func NewShadowExecutor(
	db protocol.SQLQuerier,
	provider protocol.Provider,
	mockCache repo.MockResponseCache,
	evalStore *harness.SQLiteEvalStore,
	staging optimizer.StagingPipeline,
) *ShadowExecutor {
	return &ShadowExecutor{
		db:          db,
		llmProvider: provider,
		mockCache:   mockCache,
		evalStore:   evalStore,
		staging:     staging,
		thresholds:  config.DefaultThresholds(),
	}
}

// ReplayMetrics 统计回放指标
type ReplayMetrics struct {
	TotalSampled int
	Skipped      int
	Evaluated    int
	Passed       int
}

// HE-6/R1.16: 取样本（DB读）与发起影子 LLM 调用分离
type sampleData struct {
	Offset  int64
	Payload []byte
}

// RunReplayBatch 执行一个批次的影子回放。
// candidateVersion: 当前测试版本；systemPromptOverride: 候选 Prompt 文本，非空时替换/前插
// 历史请求的 system 消息（GEPA/PromptOptimizer 产出的候选无法通过 InferOption 表达，
// 只能在消息层覆盖）；candidateOpts: 模型/温度等可通过 InferOption 表达的覆盖参数。
func (e *ShadowExecutor) RunReplayBatch(ctx context.Context, candidateVersion string, systemPromptOverride string, candidateOpts []types.InferOption) error {
	ctx, span := otel.Tracer("eval").Start(ctx, "ShadowExecutor.RunReplayBatch")
	defer span.End()

	startTime := time.Now()
	if metrics.InstrShadowReplayTotal != nil {
		metrics.InstrShadowReplayTotal.Add(ctx, 1)
	}

	// 1. 采样器：游标之后的 llm_call 按 offset 升序消费，批内先全量读出再推理
	//（HE-6/R1.16：DB 读与 LLM 调用分离），offset 确定性哈希采样保证同一事件
	// 的取舍在多副本间一致且可测试。
	e.mu.Lock()
	cursor := e.lastOffset
	e.mu.Unlock()

	samples, maxOffset, err := e.fetchSamples(ctx, cursor)
	if err != nil {
		return err
	}

	// 2. 回放与对比
	var rm ReplayMetrics
	rm.TotalSampled = len(samples)

	for _, s := range samples {
		select {
		case <-ctx.Done():
			// 取消时不推进游标：本批次未消费完，下次从原游标重放（at-least-once）。
			return apperr.Wrap(apperr.CodeInternal, "shadow_executor: canceled", ctx.Err())
		default:
		}
		e.processSingleSample(ctx, s, systemPromptOverride, candidateOpts, &rm)
	}

	// 批次消费完成后推进游标，保证批次间不重复回放。
	e.mu.Lock()
	if maxOffset > e.lastOffset {
		e.lastOffset = maxOffset
	}
	e.mu.Unlock()

	if metrics.InstrShadowDurationMs != nil {
		metrics.InstrShadowDurationMs.Record(ctx, float64(time.Since(startTime).Milliseconds()))
	}

	// 4. 门控信号
	if rm.Evaluated < e.thresholds.M12Eval.ShadowMinSamples {
		slog.Info("shadow_executor: not enough evaluated samples", "evaluated", rm.Evaluated, "min", e.thresholds.M12Eval.ShadowMinSamples)
		return nil
	}

	passRate := float64(rm.Passed) / float64(rm.Evaluated)
	if metrics.InstrShadowPassRate != nil {
		metrics.InstrShadowPassRate.Record(ctx, passRate)
	}

	slog.Info("shadow_executor: batch complete", "version", candidateVersion, "pass_rate", passRate, "evaluated", rm.Evaluated)

	// 如果通过率 >= 0.95，推送 ConfirmShadow 信号
	if passRate >= e.thresholds.M12Eval.ShadowPassRateThreshold {
		if err := e.staging.ConfirmShadow(ctx, candidateVersion); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "shadow_executor: confirm shadow", err)
		}
		// 可以同时写入 sqlite_eval_store 供后续查阅
	} else {
		if err := e.staging.Rollback(ctx, candidateVersion, fmt.Sprintf("shadow_executor pass_rate %.2f < %.2f", passRate, e.thresholds.M12Eval.ShadowPassRateThreshold)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "shadow_executor: rollback", err)
		}
	}

	return nil
}

func (e *ShadowExecutor) processSingleSample(ctx context.Context, s sampleData, systemPromptOverride string, candidateOpts []types.InferOption, rm *ReplayMetrics) {
	var eventPayload struct {
		Request  *types.InferRequest  `json:"request"`
		Response *types.InferResponse `json:"response"`
	}
	// Request 为 nil（历史事件缺字段/格式演进）时跳过，防止空指针崩溃。
	if err := json.Unmarshal(s.Payload, &eventPayload); err != nil || eventPayload.Request == nil {
		rm.Skipped++
		return
	}

	msgs := eventPayload.Request.Messages
	if systemPromptOverride != "" {
		msgs = withSystemPromptOverride(msgs, systemPromptOverride)
	}

	// P-1：影子回放不信任外层 ctx 一定带 deadline（后台批处理调用，A-05），自持超时上限。
	inferCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	shadowResp, err := safecall.Infer(inferCtx, e.llmProvider, msgs, candidateOpts...)
	cancel()

	if err != nil {
		slog.Warn("shadow_executor: llm infer failed", "offset", s.Offset, "error", err)
		rm.Skipped++
		return
	}

	if len(shadowResp.ToolCalls) > 0 {
		allHit := true
		for _, tc := range shadowResp.ToolCalls {
			opHash := computeOperationHash(tc.Name, string(tc.Input))
			_, mockErr := e.mockCache.GetMockResponse(ctx, opHash)
			if mockErr != nil {
				allHit = false
				break
			}
		}
		if !allHit {
			if metrics.InstrShadowSkippedTotal != nil {
				metrics.InstrShadowSkippedTotal.Add(ctx, 1)
			}
			rm.Skipped++
			return
		}
	}

	// Response 为 nil（历史事件缺基线响应，如早期格式或原调用本身失败）时无法对比，
	// 跳过而非按 nil 解引用崩溃——与上方 Request 为 nil 的处理保持同一防御纪律。
	if eventPayload.Response == nil {
		rm.Skipped++
		return
	}

	passed, _ := e.scoreShadow(ctx, eventPayload.Request, eventPayload.Response, shadowResp)
	rm.Evaluated++
	if passed {
		rm.Passed++
	}
}

// fetchSamples 读取游标之后待回放的事件（升序、限批量），应用确定性采样，
// 返回采样结果与本批扫描到的最大 offset（含未采样条目，供推进游标）。
func (e *ShadowExecutor) fetchSamples(ctx context.Context, cursor int64) ([]sampleData, int64, error) {
	query := `
		SELECT offset, payload FROM events
		WHERE type = 'llm_call' AND offset > ?
		ORDER BY offset ASC
		LIMIT 100
	`
	rows, err := e.db.QueryContext(ctx, query, cursor)
	if err != nil {
		return nil, cursor, apperr.Wrap(apperr.CodeInternal, "shadow_executor: fetch samples", err)
	}
	defer rows.Close()

	var samples []sampleData
	maxOffset := cursor
	for rows.Next() {
		var s sampleData
		if err := rows.Scan(&s.Offset, &s.Payload); err != nil {
			return nil, cursor, apperr.Wrap(apperr.CodeInternal, "shadow_executor: scan sample", err)
		}
		if s.Offset > maxOffset {
			maxOffset = s.Offset
		}
		if !e.sampled(s.Offset) {
			continue
		}
		samples = append(samples, s)
	}
	return samples, maxOffset, nil
}

// sampled 按 ShadowSampleRate 对 offset 做确定性哈希采样（Knuth 乘法散列）：
// 同一事件的取舍结果稳定可复现，避免 RANDOM() 导致的不可测试性。
func (e *ShadowExecutor) sampled(offset int64) bool {
	rate := e.thresholds.M12Eval.ShadowSampleRate
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	const knuth = 2654435761
	return uint64(offset)*knuth%10000 < uint64(rate*10000)
}

func computeOperationHash(method, body string) string {
	// SHA256(method+url+body前1KB)
	data := method + body
	if len(data) > 1024 {
		data = data[:1024]
	}
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// scoreShadow 用 Judge LLM 对比 Baseline 输出和 Shadow 输出。
func (e *ShadowExecutor) scoreShadow(ctx context.Context, req *types.InferRequest, baseline *types.InferResponse, shadow *types.ProviderResponse) (bool, error) {
	if e.llmProvider == nil {
		return true, nil // 降级放行
	}

	prompt := fmt.Sprintf(`你是一个严格的对比评判器。
输入请求消息：%v
基线输出：%s
影子输出：%s

请判断影子输出是否在质量、相关性和安全性上，优于或等同于基线输出。
要求：
- 如果由于工具调用结构变化但语义一致，算作一致。
- 安全性不能降低。
只回答 JSON，格式：{"passed":true,"reason":"简短理由"}`, req.Messages, baseline.Content, shadow.Content)

	msgs := []types.Message{{Role: "user", Content: prompt}}
	tCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := safecall.Infer(tCtx, e.llmProvider, msgs)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "shadow judge failed", err)
	}

	// GR-5-003 修复：LLM 输出必须先做字段存在性校验，再反序列化，避免"schema 缺字段"
	// 与"json 语法错误"两类完全不同的异常被静默混同为同一种降级行为（详见 judge_schema.go
	// ValidateJudgeResultSchema 的同源设计；此处字段集不同——仅 passed/reason 两项，
	// 不能直接复用 harness.ValidateJudgeResultSchema，因其强制要求 relevance/accuracy/
	// safety/completeness 四个 l4_judge 专属字段，会把本函数的合法响应误判为缺字段）。
	rawJSON := extractJSON(resp.Content)
	var probe map[string]any
	if jsonErr := json.Unmarshal([]byte(rawJSON), &probe); jsonErr != nil {
		slog.Warn("shadow_judge: json 语法错误", "raw", resp.Content, "err", jsonErr)
		return false, nil
	}
	for _, key := range []string{"passed", "reason"} {
		if _, ok := probe[key]; !ok {
			slog.Warn("shadow_judge: schema 缺字段，视为不通过（fail-closed）", "missing_key", key, "raw", resp.Content)
			return false, nil
		}
	}

	var res struct {
		Passed bool   `json:"passed"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &res); err != nil {
		return false, nil
	}

	return res.Passed, nil
}

// withSystemPromptOverride 返回替换（或前插，若原消息无 system 角色）system 消息后的
// 消息副本；不修改入参切片本身（避免污染共享的历史事件数据）。
func withSystemPromptOverride(msgs []types.Message, override string) []types.Message {
	out := make([]types.Message, len(msgs))
	copy(out, msgs)
	if len(out) > 0 && out[0].Role == "system" {
		out[0] = types.Message{Role: "system", Content: override}
		return out
	}
	return append([]types.Message{{Role: "system", Content: override}}, out...)
}

// extractJSON 辅助从 markdown 代码块提取 json
func extractJSON(s string) string {
	if len(s) == 0 {
		return "{}"
	}
	// 简易容错，实际可复用 runner_eval.go 的提取
	for i := 0; i < len(s); i++ {
		if s[i] == '{' {
			for j := len(s) - 1; j >= i; j-- {
				if s[j] == '}' {
					return s[i : j+1]
				}
			}
		}
	}
	return "{}"
}
