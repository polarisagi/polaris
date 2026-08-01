// Package llmgen 提供 Skill/Plugin 生成器共用的"有界重试 + 熔断 + tracing/
// metrics"结构化 LLM 生成骨架（阶段03 R-06）。
//
// 背景：skill_creator.go / plugin_creator.go 此前对 LLM 生成的 JSON 只做一次
// json.Unmarshal，失败即抛，无重试、无熔断、无 tracing/metrics（GeneratePlugin/
// GenerateSkill 完全脱离 HE-1 可观测性）。两处代码结构逐字重复，本包抽出通用
// 骨架，具体的"如何调用 LLM""如何解析/校验结构体"仍由各调用方以闭包形式提供
// （不反向依赖 skill/plugin 包，符合 HE-3 接口在调用方定义原则——本包反过来是
// 被调用方，闭包参数即调用方注入其领域逻辑的方式）。
package llmgen

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	// maxStructuredRetries 上限重试次数（共 maxStructuredRetries+1 次调用）。
	// LLM 结构化输出失败通常是模型能力问题而非瞬时故障，无限重试只会烧 token（A-08）。
	maxStructuredRetries = 2
	// breakerThreshold 连续失败达到此值触发熔断。
	breakerThreshold = 5
	// breakerCooldown 熔断冷却时长。
	breakerCooldown = 5 * time.Minute
)

// GenerateFunc 是调用方提供的单次 LLM 生成闭包：入参为当前 system/user prompt
// （重试时 userPrompt 已被追加上一次失败原因），返回生成的原始文本。
// 调用方在此闭包内部选择走 Provider 结构化输出（如 JSON 模式）还是普通生成——
// StructuredGenerator 本身不关心 Provider 能力，只负责重试/熔断/可观测性骨架。
type GenerateFunc func(ctx context.Context, systemPrompt, userPrompt string) (string, error)

// ValidateFunc 校验/解析一次生成结果。成功时应在闭包内部把解析出的结构体写入
// 调用方自己持有的外部变量并返回 nil；失败返回的 error 会被摘要后回灌进下一次
// 重试的 prompt，提高模型自我纠正的成功率。
type ValidateFunc func(raw string) error

// StructuredGenerator 有界重试 + 熔断地向 LLM 索取结构化 JSON。
// kind 是调用方类型的固定枚举标签（如 "skill"/"plugin"），用于 tracing span
// 名与 metrics label——必须是调用方硬编码的常量，不得传入任何用户可控值。
type StructuredGenerator struct {
	kind string
	// backoff 重试间隔配置，默认 llmparent.DefaultBackoff()。未导出——生产
	// 调用方无需关心，仅同包测试为避免真实等待（Base 默认 5s）而覆盖。
	backoff llmparent.BackoffConfig

	mu                  sync.Mutex
	consecutiveFailures int
	cooldownUntil       time.Time
}

// NewStructuredGenerator 构造一个按 kind 独立维护熔断状态的生成器。
// 默认退避为 llmparent.DefaultBackoff()（Base=5s，面向网络级/限流重试场景）；
// 结构化 JSON 纠错重试通常更适合更短间隔，调用方可用 WithBackoff 覆盖。
func NewStructuredGenerator(kind string) *StructuredGenerator {
	return &StructuredGenerator{kind: kind, backoff: llmparent.DefaultBackoff()}
}

// WithBackoff 覆盖默认重试退避配置。返回自身，支持链式调用。
func (g *StructuredGenerator) WithBackoff(cfg llmparent.BackoffConfig) *StructuredGenerator {
	g.backoff = cfg
	return g
}

// Generate 有界重试地向 LLM 索取结构化 JSON，驱动熔断状态机与 tracing/metrics。
// 重试耗尽或熔断开启时返回非 nil error；validate 成功时返回 nil。
func (g *StructuredGenerator) Generate(ctx context.Context, systemPrompt, userPrompt string, gen GenerateFunc, validate ValidateFunc) error {
	start := time.Now()
	ctx, span := otel.Tracer("polaris/extension").Start(ctx, "extension.generate_"+g.kind)
	defer span.End()
	span.SetAttributes(attribute.String("kind", g.kind))

	if blocked, remaining := g.breakerBlocked(); blocked {
		span.AddEvent("circuit_open")
		metrics.RecordExtensionLLMCall(ctx, g.kind, "circuit_open", float64(time.Since(start).Milliseconds()))
		return apperr.New(apperr.CodeResourceExhausted,
			"extension: structured generation circuit open for kind="+g.kind+", cooling down for "+remaining.Round(time.Second).String())
	}

	prompt := userPrompt
	var lastErr error

	for attempt := 0; attempt <= maxStructuredRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(g.backoff.Delay(attempt)):
			case <-ctx.Done():
				return apperr.Wrap(apperr.CodeCancelled, "extension: structured generation cancelled", ctx.Err())
			}
		}

		raw, err := gen(ctx, systemPrompt, prompt)
		if err != nil {
			lastErr = err
			slog.WarnContext(ctx, "extension: llm generate failed", "kind", g.kind, "attempt", attempt+1, "err", err)
			prompt = userPrompt + "\n\n[Retry] Your previous response failed: " + err.Error() + ". Please try again."
			continue
		}

		if verr := validate(raw); verr != nil {
			lastErr = verr
			slog.WarnContext(ctx, "extension: llm structured output invalid", "kind", g.kind, "attempt", attempt+1, "err", verr)
			prompt = userPrompt + "\n\n[Retry] Your previous response was invalid: " + verr.Error() +
				". Please try again and output ONLY raw JSON matching the schema, no Markdown wrappers."
			continue
		}

		g.recordSuccess()
		metrics.RecordExtensionLLMCall(ctx, g.kind, "success", float64(time.Since(start).Milliseconds()))
		return nil
	}

	g.recordFailure()
	metrics.RecordExtensionStructuredFailure(ctx, g.kind)
	metrics.RecordExtensionLLMCall(ctx, g.kind, "failure", float64(time.Since(start).Milliseconds()))
	span.RecordError(lastErr)
	return apperr.Wrap(apperr.CodeInternal, "extension: structured generation exhausted retries for kind="+g.kind, lastErr)
}

// breakerBlocked 判定当前是否处于熔断冷却期。冷却期已过则重置计数（半开），
// 允许下一次尝试重新累计失败次数。
func (g *StructuredGenerator) breakerBlocked() (blocked bool, remaining time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.consecutiveFailures < breakerThreshold {
		return false, 0
	}
	remaining = time.Until(g.cooldownUntil)
	if remaining <= 0 {
		g.consecutiveFailures = 0
		return false, 0
	}
	return true, remaining
}

func (g *StructuredGenerator) recordSuccess() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.consecutiveFailures = 0
}

func (g *StructuredGenerator) recordFailure() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.consecutiveFailures++
	if g.consecutiveFailures >= breakerThreshold {
		g.cooldownUntil = time.Now().Add(breakerCooldown)
		slog.Warn("extension: structured generation circuit opened", "kind", g.kind, "cooldown_until", g.cooldownUntil)
	}
}
