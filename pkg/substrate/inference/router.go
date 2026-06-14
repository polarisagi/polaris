// Package inference 实现 M1 Inference Runtime 路由层。
// 架构文档: docs/arch/M01-Inference-Runtime.md §3-§4
//
// 设计约束:
//   - ProviderRegistry: 注册/注销 Provider，HealthScore 动态权重
//   - InferenceRouter.Route(): 权重 = 可用性×0.4 + 延迟×0.3 + 成本×0.2 + 质量×0.1
//   - CircuitBreaker: 连续失败 → Open(冷却) → HalfOpen → 探测（参数见 §4.5）
//   - SSE 帧归一化: OpenAI/Anthropic/DeepSeek → 统一 StreamEvent
//   - API Key JIT: 使用后 memclr 清零（防 heap dump 泄露）
package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/polarisagi/polaris/internal/config"
	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// ─── CircuitBreaker ────────────────────────────────────────────────────────────

// circuitState 熔断器状态。
type circuitState int32

const (
	circuitClosed   circuitState = iota // 正常放行
	circuitOpen                         // 拒绝请求
	circuitHalfOpen                     // 探测恢复
)

// circuitBreaker 连续失败 → Open(冷却期) → HalfOpen 探测。
// 架构文档: M01 §4.5（参数权威源 spec/state.yaml §m1_router.circuit_breaker_*）
type circuitBreaker struct {
	state       atomic.Int32
	failures    atomic.Int32
	openUntil   atomic.Int64 // unix nano
	maxFailures int32
	openDur     time.Duration
}

// newCircuitBreaker 按 M1RouterThresholds 配置创建熔断器。
// 零值字段回退 spec/state.yaml 默认值（5 次失败 / 10s 冷却）。
func newCircuitBreaker(cfg config.M1RouterThresholds) *circuitBreaker {
	maxFail := int32(cfg.CircuitBreakerFailureCount)
	if maxFail <= 0 {
		maxFail = 5 // spec/state.yaml §m1_router.circuit_breaker_failure_count 默认值
	}
	cooldown := time.Duration(cfg.CircuitBreakerCooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 10 * time.Second // spec/state.yaml §m1_router.circuit_breaker_cooldown_seconds 默认值
	}
	cb := &circuitBreaker{maxFailures: maxFail, openDur: cooldown}
	cb.state.Store(int32(circuitClosed))
	return cb
}

func (cb *circuitBreaker) Allow() bool {
	switch circuitState(cb.state.Load()) {
	case circuitClosed:
		return true
	case circuitOpen:
		if time.Now().UnixNano() > cb.openUntil.Load() {
			cb.state.Store(int32(circuitHalfOpen))
			return true // 允许一次探测
		}
		return false
	case circuitHalfOpen:
		return true
	}
	return false
}

func (cb *circuitBreaker) RecordSuccess() (recovered bool) {
	prev := circuitState(cb.state.Load())
	cb.failures.Store(0)
	cb.state.Store(int32(circuitClosed))
	return prev == circuitHalfOpen
}

func (cb *circuitBreaker) RecordFailure() {
	n := cb.failures.Add(1)
	if n >= cb.maxFailures {
		cb.state.Store(int32(circuitOpen))
		cb.openUntil.Store(time.Now().Add(cb.openDur).UnixNano())
		cb.failures.Store(0)
	}
}

// ─── ProviderEntry ─────────────────────────────────────────────────────────────

// providerEntry 封装单个 Provider 的运行时状态。
type providerEntry struct {
	provider    protocol.Provider
	name        string
	role        string // general | default | reasoning
	displayName string // 用于 WebUI 展示的友好名称
	cb          *circuitBreaker
	mu          sync.RWMutex
	p95ms       float64 // P95 延迟（指数移动平均）
	successRate float64 // 成功率（指数移动平均，初始 1.0）
	// costScore: 由 ProviderCapabilities.CostPer1KInput 驱动（值越小越好）
}

func newProviderEntry(name, displayName string, p protocol.Provider, cfg config.M1RouterThresholds) *providerEntry {
	return &providerEntry{
		name:        name,
		displayName: displayName,
		provider:    p,
		cb:          newCircuitBreaker(cfg),
		p95ms:       200, // 初始 P95 假设 200ms
		successRate: 1.0,
	}
}

// healthScore 综合健康评分 = 可用性×0.4 + 延迟×0.3 + 成本×0.2 + 质量×0.1
// 延迟得分 = max(0, 1 - p95ms/5000)；成本得分 = max(0, 1 - costPer1KInput/10)
func (e *providerEntry) healthScore() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	caps := e.provider.Capabilities()
	latencyScore := max64(0, 1.0-e.p95ms/5000.0)
	costScore := max64(0, 1.0-caps.CostPer1KInput/10.0)
	return e.successRate*0.4 + latencyScore*0.3 + costScore*0.2 + 0.1
}

func (e *providerEntry) recordLatency(ms float64) {
	e.mu.Lock()
	// 指数移动平均 α=0.1（对突刺平滑）
	e.p95ms = e.p95ms*0.9 + ms*0.1
	e.mu.Unlock()
}

func (e *providerEntry) recordOutcome(success bool, onRecovery func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if success {
		e.successRate = e.successRate*0.95 + 0.05
		if recovered := e.cb.RecordSuccess(); recovered && onRecovery != nil {
			go onRecovery() // 异步触发，不阻断热路径
		}
	} else {
		e.successRate = e.successRate * 0.95
		e.cb.RecordFailure()
	}
}

// ─── ProviderRegistry ──────────────────────────────────────────────────────────

// ProviderRegistry 注册/注销 Provider，支持热更新。
type ProviderRegistry struct {
	mu         sync.RWMutex
	entries    map[string]*providerEntry
	onRecovery func(providerName string) // 可选：Provider 熔断恢复时的回调
	cfg        config.M1RouterThresholds // 熔断器配置（来自 M1RouterThresholds TOML）
}

func NewProviderRegistry(cfg config.M1RouterThresholds) *ProviderRegistry {
	return &ProviderRegistry{
		entries: make(map[string]*providerEntry),
		cfg:     cfg,
	}
}

// InjectRecoveryHandler 注入 Provider 恢复回调，由上层（如 InferenceRouter）在初始化时调用。
// fn 在 circuitBreaker HalfOpen→Closed 时触发，providerName 为恢复的 Provider 名称。
func (r *ProviderRegistry) InjectRecoveryHandler(fn func(providerName string)) {
	r.mu.Lock()
	r.onRecovery = fn
	r.mu.Unlock()
}

func (r *ProviderRegistry) Register(name, displayName string, p protocol.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[name] = newProviderEntry(name, displayName, p, r.cfg)
}

func (r *ProviderRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, name)
}

// UnregisterAll 清空所有注册项，用于热重载前的清理。
func (r *ProviderRegistry) UnregisterAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = make(map[string]*providerEntry)
}

// RegisterWithRole 注册带角色标记的 Provider（general | default | reasoning）。
func (r *ProviderRegistry) RegisterWithRole(name, displayName, role string, p protocol.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := newProviderEntry(name, displayName, p, r.cfg)
	e.role = role
	r.entries[name] = e
}

// BestForRole 返回指定角色下 healthScore 最高的可用 entry。
// 若 role 为空或无匹配则回退到全局 best()。
func (r *ProviderRegistry) BestForRole(role string, req *protocol.InferRequest) *providerEntry {
	if role == "" || role == "general" {
		return r.best(req)
	}

	chosen := r.findBestByRole(role)
	if chosen == nil {
		return r.best(req)
	}
	return chosen
}

func (r *ProviderRegistry) findBestByRole(role string) *providerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var chosen *providerEntry
	bestScore := -1.0
	for _, e := range r.entries {
		if !e.cb.Allow() {
			continue
		}
		if e.role != role && e.role != "general" {
			continue
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	return chosen
}

// PickProvider 返回指定角色 healthScore 最优的 Provider，供外部直接发起推理。
// 若无可用 Provider 返回 nil。
func (r *ProviderRegistry) PickProvider(role string) protocol.Provider {
	e := r.BestForRole(role, nil)
	if e == nil {
		return nil
	}
	return e.provider
}

// PickProviderByRecordID 尝试通过 model 记录的 UUID 前缀寻找对应的 Provider。
func (r *ProviderRegistry) PickProviderByRecordID(mID string) protocol.Provider {
	if mID == "" {
		return nil
	}
	suffix := mID
	if len(suffix) >= 8 {
		suffix = suffix[:8]
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	var chosen *providerEntry
	bestScore := -1.0
	for name, e := range r.entries {
		if !strings.HasSuffix(name, "/"+suffix) {
			continue
		}
		if !e.cb.Allow() {
			continue
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	if chosen != nil {
		return chosen.provider
	}
	return nil
}

// PickProviderName 返回指定角色最优 Provider 的注册名（含模型标识），供状态展示。
func (r *ProviderRegistry) PickProviderName(role string) string {
	e := r.BestForRole(role, nil)
	if e == nil {
		return ""
	}
	if e.displayName != "" {
		return e.displayName
	}
	return e.name
}

// best 按 healthScore 降序返回第一个 CircuitBreaker 允许且满足多模态能力要求的 entry。
func (r *ProviderRegistry) best(req *protocol.InferRequest) *providerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	needsVision := req != nil && req.HasImageParts()
	needsVideo := req != nil && req.HasVideoParts()

	var chosen *providerEntry
	bestScore := -1.0
	for _, e := range r.entries {
		if !e.cb.Allow() {
			continue
		}
		caps := e.provider.Capabilities()
		if needsVision && !caps.SupportsVision {
			continue
		}
		if needsVideo && !caps.SupportsVideo {
			continue
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	return chosen
}

// ─── InferenceRouter ───────────────────────────────────────────────────────────

// InferenceRouter 实现 protocol.Provider，对上层透明地完成多厂商路由。
// 架构文档: docs/arch/M01-Inference-Runtime.md §4
type InferenceRouter struct {
	registry     *ProviderRegistry
	rateTracker  *RateLimitTracker
	client       *http.Client
	outboxWriter protocol.OutboxWriter
}

func (ir *InferenceRouter) InjectOutboxWriter(w protocol.OutboxWriter) {
	ir.outboxWriter = w
}

var _ protocol.Provider = (*InferenceRouter)(nil)

func NewInferenceRouter(reg *ProviderRegistry, dialer protocol.SafeDialer) *InferenceRouter {
	transport := &http.Transport{}
	if dialer != nil {
		transport.DialContext = dialer.DialContext
	}
	tracker := NewRateLimitTracker()
	ir := &InferenceRouter{
		registry:    reg,
		rateTracker: tracker,
		client: &http.Client{
			Transport: &RateLimitCapturingTransport{
				Inner:   transport,
				Tracker: tracker,
			},
			Timeout: 120 * time.Second,
		},
	}
	reg.InjectRecoveryHandler(func(providerName string) {
		// 向 outbox 投递 m4_provider_recovery 事件，唤醒因 provider_suspended 挂起的 Agent
		if ir.outboxWriter == nil {
			return
		}
		payload, _ := json.Marshal(map[string]string{
			"event_type":    "m4_provider_recovery",
			"provider_name": providerName,
		})
		_ = ir.outboxWriter.Write(context.Background(), protocol.OutboxEntry{
			TargetEngine:   "agent_kernel",
			Operation:      "provider_recovery",
			Payload:        payload,
			IdempotencyKey: "recovery:" + providerName + ":" + strconv.FormatInt(time.Now().Unix(), 10),
		})
	})
	return ir
}

func (ir *InferenceRouter) ModelID() string {
	entry := ir.registry.best(nil)
	if entry == nil || entry.provider == nil {
		return "unknown"
	}
	return entry.provider.ModelID()
}

// Infer 路由单次请求到最优 Provider，失败时 failover 至次优。
func (ir *InferenceRouter) Infer(ctx context.Context, msgs []protocol.Message, opts ...protocol.InferOption) (*protocol.ProviderResponse, error) {
	options := &protocol.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &protocol.InferRequest{
		Messages:        msgs,
		Model:           options.Model,
		MaxTokens:       options.MaxTokens,
		Tools:           options.Tools,
		ThinkingMode:    options.ThinkingMode,
		Temperature:     options.Temperature,
		ResponseFormat:  options.ResponseFormat,
		ReasoningEffort: options.ReasoningEffort,
	}

	// 统一预处理：降采样超规格图片、PNG/GIF→JPEG 格式转换
	// 覆盖所有调用方（Gateway / Cognition Kernel / Swarm / Extensions）
	normalizeInferRequest(req)
	entry := ir.registry.best(req)
	if entry == nil {
		return nil, perrors.New(perrors.CodeInternal, "inference_router: no available providers")
	}
	start := time.Now()
	resp, err := entry.provider.Infer(ctx, msgs, opts...)
	ms := float64(time.Since(start).Milliseconds())
	entry.recordLatency(ms)
	entry.recordOutcome(err == nil, func() {
		ir.registry.mu.RLock()
		fn := ir.registry.onRecovery
		name := entry.name
		ir.registry.mu.RUnlock()
		if fn != nil {
			fn(name)
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			// ctx 已取消或超时，不发起 failover（节省 Token 和 goroutine）
			return nil, err
		}
		// Failover: 尝试次优 Provider
		return ir.failover(ctx, msgs, opts, req, entry.name)
	}
	// M3 埋点：LLM 调用成功后记录指标
	if resp != nil {
		caps := entry.provider.Capabilities()
		costUSD := float64(resp.Usage.InputTokens)*caps.CostPer1KInput/1000.0 +
			float64(resp.Usage.OutputTokens)*caps.CostPer1KOutput/1000.0 +
			float64(resp.Usage.CacheHitTokens)*caps.CostPer1KCacheHit/1000.0
		observability.RecordLLMCall(ctx,
			entry.name, resp.Model, "success", ms,
			resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheHitTokens,
			costUSD,
		)
	}
	return resp, nil
}

// StreamInfer 路由流式请求，内嵌延迟记录与 Failover。
func (ir *InferenceRouter) StreamInfer(ctx context.Context, msgs []protocol.Message, opts ...protocol.InferOption) (<-chan protocol.StreamEvent, error) {
	options := &protocol.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &protocol.InferRequest{
		Messages:        msgs,
		Model:           options.Model,
		MaxTokens:       options.MaxTokens,
		Tools:           options.Tools,
		ThinkingMode:    options.ThinkingMode,
		Temperature:     options.Temperature,
		ResponseFormat:  options.ResponseFormat,
		ReasoningEffort: options.ReasoningEffort,
	}

	// 统一预处理：与 Infer 路径一致，确保流式和非流式路径均受益
	normalizeInferRequest(req)
	entry := ir.registry.best(req)
	if entry == nil {
		return nil, perrors.New(perrors.CodeInternal, "inference_router: no available providers")
	}
	start := time.Now()
	ch, err := entry.provider.StreamInfer(ctx, msgs, opts...)
	entry.recordLatency(float64(time.Since(start).Milliseconds()))
	entry.recordOutcome(err == nil, func() {
		ir.registry.mu.RLock()
		fn := ir.registry.onRecovery
		name := entry.name
		ir.registry.mu.RUnlock()
		if fn != nil {
			fn(name)
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			// ctx 已取消或超时，不发起 failover（节省 Token 和 goroutine）
			return nil, err
		}
		// Failover: 尝试次优 Provider
		return ir.streamFailover(ctx, msgs, opts, req, entry.name)
	}
	return ch, nil
}

// streamFailover 流式路径次优选择。
func (ir *InferenceRouter) streamFailover(ctx context.Context, msgs []protocol.Message, opts []protocol.InferOption, req *protocol.InferRequest, skip string) (<-chan protocol.StreamEvent, error) {
	ir.registry.mu.RLock()
	defer ir.registry.mu.RUnlock()
	var chosen *providerEntry
	bestScore := -1.0
	for name, e := range ir.registry.entries {
		if name == skip || !e.cb.Allow() {
			continue
		}
		if req != nil {
			caps := e.provider.Capabilities()
			if req.HasImageParts() && !caps.SupportsVision {
				continue
			}
			if req.HasVideoParts() && !caps.SupportsVideo {
				continue
			}
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	if chosen == nil {
		return nil, perrors.New(perrors.CodeInternal, "inference_router: all providers failed (stream)")
	}
	ch, err := chosen.provider.StreamInfer(ctx, msgs, opts...)
	chosen.recordOutcome(err == nil, func() {
		ir.registry.mu.RLock()
		fn := ir.registry.onRecovery
		name := chosen.name
		ir.registry.mu.RUnlock()
		if fn != nil {
			fn(name)
		}
	})
	return ch, err
}

func (ir *InferenceRouter) Capabilities() protocol.ProviderCapabilities {
	// 聚合：取所有可用 Provider 能力并集
	caps := protocol.ProviderCapabilities{}
	ir.registry.mu.RLock()
	defer ir.registry.mu.RUnlock()
	for _, e := range ir.registry.entries {
		c := e.provider.Capabilities()
		if c.SupportsStreaming {
			caps.SupportsStreaming = true
		}
		if c.SupportsTools {
			caps.SupportsTools = true
		}
		if c.SupportsVision {
			caps.SupportsVision = true
		}
		if c.SupportsVideo {
			caps.SupportsVideo = true
		}
		if c.SupportsTTS {
			caps.SupportsTTS = true
		}
		if c.MaxContextTokens > caps.MaxContextTokens {
			caps.MaxContextTokens = c.MaxContextTokens
		}
	}
	return caps
}

func (ir *InferenceRouter) Tokenizer() protocol.TokenizerAdapter {
	entry := ir.registry.best(nil)
	if entry == nil {
		return &simpleTokenizer{}
	}
	return entry.provider.Tokenizer()
}

func (ir *InferenceRouter) failover(ctx context.Context, msgs []protocol.Message, opts []protocol.InferOption, req *protocol.InferRequest, skip string) (*protocol.ProviderResponse, error) {
	start := time.Now()
	ir.registry.mu.RLock()
	defer ir.registry.mu.RUnlock()
	bestScore := -1.0
	var chosen *providerEntry
	for name, e := range ir.registry.entries {
		if name == skip || !e.cb.Allow() {
			continue
		}
		if req != nil {
			caps := e.provider.Capabilities()
			if req.HasImageParts() && !caps.SupportsVision {
				continue
			}
			if req.HasVideoParts() && !caps.SupportsVideo {
				continue
			}
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	if chosen == nil {
		return nil, perrors.New(perrors.CodeInternal, "inference_router: all providers failed")
	}
	resp, err := chosen.provider.Infer(ctx, msgs, opts...)
	chosen.recordOutcome(err == nil, func() {
		ir.registry.mu.RLock()
		fn := ir.registry.onRecovery
		name := chosen.name
		ir.registry.mu.RUnlock()
		if fn != nil {
			fn(name)
		}
	})
	if err == nil && resp != nil {
		caps := chosen.provider.Capabilities()
		costUSD := float64(resp.Usage.InputTokens)*caps.CostPer1KInput/1000.0 +
			float64(resp.Usage.OutputTokens)*caps.CostPer1KOutput/1000.0 +
			float64(resp.Usage.CacheHitTokens)*caps.CostPer1KCacheHit/1000.0
		observability.RecordLLMCall(ctx,
			chosen.name, resp.Model, "failover", float64(time.Since(start).Milliseconds()),
			resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheHitTokens,
			costUSD,
		)
	}
	return resp, err
}

// ─── 工具函数 ──────────────────────────────────────────────────────────────────

// clearString API Key 使用后原地清零（防止 heap dump 泄漏敏感数据）。
// 使用 unsafe.StringData + unsafe.Slice 直接操作底层字节（Go 1.20+）。
func clearString(s *string) {
	if len(*s) == 0 {
		return
	}
	// 取底层数组指针并以 byte slice 视图覆写，不产生副本
	b := unsafe.Slice(unsafe.StringData(*s), len(*s))
	for i := range b {
		b[i] = 0
	}
	*s = ""
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// simpleTokenizer 简单 token 估算（4 字符/token），用于本地 Provider（Ollama 等）。
// 精确计算请使用 tiktokenTokenizer（OpenAI/DeepSeek 适配器的默认实现）。
type simpleTokenizer struct{}

func (t *simpleTokenizer) CountTokens(text string) int { return len(text) / 4 }
func (t *simpleTokenizer) CountTokensBatch(texts []string) []int {
	result := make([]int, len(texts))
	for i, s := range texts {
		result[i] = len(s) / 4
	}
	return result
}

// estimateRequestTokens 估算请求总 token 数，供流式 cancel 补偿用。
func (t *simpleTokenizer) estimateRequestTokens(req *protocol.InferRequest) int {
	total := 0
	for _, msg := range req.Messages {
		total += 4 + t.CountTokens(msg.Content)
		for _, p := range msg.Parts {
			if m, ok := p.(map[string]any); ok {
				if txt, ok := m["text"].(string); ok {
					total += t.CountTokens(txt)
				}
			}
		}
	}
	return total + 3
}
