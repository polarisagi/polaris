package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

var (
	ErrBatcherSaturated = apperr.New(apperr.CodeResourceExhausted, "embedding batcher saturated")
	ErrBatcherStopped   = apperr.New(apperr.CodeCancelled, "embedding batcher stopped")
)

// 优先级取值：0 为 High（交互），其余进 Low（后台批量）。
const (
	PriorityHigh = 0 // SurpriseIndex、交互式查询、同步适配器
	PriorityLow  = 1 // GraphRAG、Consolidation、回填等后台批量
)

const (
	highQueueCap          = 180
	lowQueueCap           = 76
	defaultLowMaxBatch    = 8
	defaultCallTimeout    = 30 * time.Second
	defaultBatchWindow    = 10 * time.Millisecond
	defaultHighMaxBatch   = 100
	laneNameHigh, laneLow = "high", "low"
)

// EmbeddingBatcher — Embedding API 批量调用优化器。
// 架构文档: docs/arch/M01-Inference-Runtime.md §6.1、ADR-0099
//
// High 与 Low 两条通道各自独立 flush、各自至多一个在途调用：交互请求永不在本进程
// 内排到后台大批次之后（此前单循环混装一批，实测 100 条/批 13.8s，交互嵌入稳定
// 30s 超时）。Low 单批上限收紧，约束它在串行后端上占用的时长。
type EmbeddingBatcher struct {
	batchWindow  time.Duration
	maxBatchSize int // High 单批上限（兼作"大请求直发"阈值）
	callTimeout  time.Duration
	embedFn      EmbedFn

	mu      sync.Mutex
	high    *embedLane
	low     *embedLane
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
	stopped bool
}

// EmbedFn M1 Embedding API 调用函数类型（依赖注入，可 mock）。
type EmbedFn func(ctx context.Context, texts []string, model string) ([][]float32, error)

// embedLane 单条优先级通道。pending/dedup 受 EmbeddingBatcher.mu 保护；
// callMu 串行化本通道的下游调用（flush 循环与大请求直发共用），保证至多一个在途。
type embedLane struct {
	name     string
	capacity int
	maxBatch int
	pending  []EmbedRequest
	// dedup: textHash → 等待该文本结果的 channel（扇出）。按通道隔离：跨通道共享会让
	// High 请求挂到排队中的 Low 条目上，重新引入队头阻塞。
	dedup  map[string][]chan EmbedResult
	callMu sync.Mutex
}

func newEmbedLane(name string, capacity, maxBatch int) *embedLane {
	return &embedLane{name: name, capacity: capacity, maxBatch: maxBatch, dedup: make(map[string][]chan EmbedResult)}
}

// NewEmbeddingBatcher 创建 EmbeddingBatcher，embedFn 为 M1 Embedding API（nil 则 flushBatch 报错）。
// maxBatchSize 为 High 单批上限；Low 单批上限与下游超时经 WithLaneLimits 调整。
func NewEmbeddingBatcher(batchWindow time.Duration, maxBatchSize int, embedFn EmbedFn) *EmbeddingBatcher {
	if batchWindow <= 0 {
		batchWindow = defaultBatchWindow
	}
	if maxBatchSize <= 0 {
		maxBatchSize = defaultHighMaxBatch
	}
	return &EmbeddingBatcher{
		batchWindow:  batchWindow,
		maxBatchSize: maxBatchSize,
		callTimeout:  defaultCallTimeout,
		embedFn:      embedFn,
		high:         newEmbedLane(laneNameHigh, highQueueCap, maxBatchSize),
		low:          newEmbedLane(laneLow, lowQueueCap, min(defaultLowMaxBatch, maxBatchSize)),
	}
}

// WithLaneLimits 设置 Low 单批上限与单次下游调用超时（ADR-0099，阈值
// thresholds.m1_router.embed.*）。须在 Start 前调用；非正值保持默认。
func (b *EmbeddingBatcher) WithLaneLimits(lowMaxBatch int, callTimeout time.Duration) *EmbeddingBatcher {
	b.mu.Lock()
	defer b.mu.Unlock()
	if lowMaxBatch > 0 {
		b.low.maxBatch = min(lowMaxBatch, b.maxBatchSize)
	}
	if callTimeout > 0 {
		b.callTimeout = callTimeout
	}
	return b
}

// Start 启动两条通道各自的 flush 循环。
func (b *EmbeddingBatcher) Start(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return
	}
	b.started = true
	ctx, b.cancel = context.WithCancel(ctx)
	b.done = make(chan struct{})

	var wg sync.WaitGroup
	for _, ln := range []*embedLane{b.high, b.low} {
		wg.Add(1)
		concurrent.SafeGo(ctx, "embedding_batcher_"+ln.name, func(ctx context.Context) {
			defer wg.Done()
			b.runLane(ctx, ln)
		})
	}
	concurrent.SafeGo(ctx, "embedding_batcher_stop", func(context.Context) {
		wg.Wait()
		b.failPending(ErrBatcherStopped)
		close(b.done)
	})
}

func (b *EmbeddingBatcher) runLane(ctx context.Context, ln *embedLane) {
	ticker := time.NewTicker(b.batchWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.flushLane(ctx, ln)
		}
	}
}

// Stop 停止 flush 循环并等待其退出；未 Start 或重复调用均安全，nil 接收者安全。
func (b *EmbeddingBatcher) Stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	cancel, done := b.cancel, b.done
	b.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// failPending 标记停止并以 err 回告所有排队中的等待者。
func (b *EmbeddingBatcher) failPending(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	res := EmbedResult{Error: err}
	for _, ln := range []*embedLane{b.high, b.low} {
		for key, chs := range ln.dedup {
			for _, ch := range chs {
				ch <- res
			}
			delete(ln.dedup, key)
		}
		ln.pending = nil
	}
}

// flushLane 取出本通道至多 maxBatch 条并调用下游，结果扇出给全部等待者。
func (b *EmbeddingBatcher) flushLane(ctx context.Context, ln *embedLane) {
	b.mu.Lock()
	n := min(len(ln.pending), ln.maxBatch)
	if n == 0 {
		b.mu.Unlock()
		return
	}
	batch := append([]EmbedRequest(nil), ln.pending[:n]...)
	ln.pending = append(ln.pending[:0], ln.pending[n:]...)
	b.mu.Unlock()

	texts := make([]string, len(batch))
	for i, req := range batch {
		texts[i] = req.Text
	}
	// 批内按首条请求的模型调用：同一进程的嵌入模型全局唯一（DynamicEmbedder）。
	results, batchErr := b.callLane(ctx, ln, texts, batch[0].Model)

	b.mu.Lock()
	defer b.mu.Unlock()
	for i, req := range batch {
		res := EmbedResult{Error: batchErr}
		if batchErr == nil {
			res = EmbedResult{Error: apperr.New(apperr.CodeInternal, "missing result")}
			if i < len(results) {
				res = results[i]
			}
		}
		key := textHash(req.Text)
		for _, ch := range ln.dedup[key] {
			ch <- res
		}
		delete(ln.dedup, key)
	}
}

// callLane 串行化本通道的下游调用并施加单次超时：后端挂起只让该批失败，
// 不再冻结整条队列（此前用批处理器的长生命周期 ctx，无上限）。
func (b *EmbeddingBatcher) callLane(ctx context.Context, ln *embedLane, texts []string, model string) ([]EmbedResult, error) {
	ln.callMu.Lock()
	defer ln.callMu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, b.callTimeout)
	defer cancel()
	return b.flushBatch(cctx, texts, model)
}

// textHash 为文本生成去重键（SHA-256 前 16 字节，碰撞率可忽略）。
func textHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:16])
}

// EmbedRequest 单次 embedding 请求。
type EmbedRequest struct {
	Text     string
	Model    string
	Priority int
	ResultCh chan EmbedResult
}

// EmbedResult embedding 结果。
type EmbedResult struct {
	Vector []float32
	Error  error
}

func (b *EmbeddingBatcher) laneOf(priority int) *embedLane {
	if priority == PriorityHigh {
		return b.high
	}
	return b.low
}

// Embed 提交 embedding 请求并等待结果。
//
// len(texts) 达到本通道单批上限 → 按上限切块直发（仍经 callMu 串行，与 flush 循环
// 共享"至多一个在途"约束）；否则全部入队后统一等待——此前逐条入队、逐条等待，
// N 条文本要串行等 N 个 flush 周期。
func (b *EmbeddingBatcher) Embed(ctx context.Context, texts []string, model string, priority int) ([]EmbedResult, error) {
	ln := b.laneOf(priority)
	if len(texts) >= ln.maxBatch {
		return b.embedDirect(ctx, ln, texts, model)
	}

	chans := make([]chan EmbedResult, len(texts))
	for i, text := range texts {
		chans[i] = make(chan EmbedResult, 1)
		b.enqueue(ln, EmbedRequest{Text: text, Model: model, Priority: priority, ResultCh: chans[i]})
	}
	results := make([]EmbedResult, len(texts))
	for i, ch := range chans {
		select {
		case r := <-ch:
			results[i] = r
		case <-ctx.Done():
			return results, ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
		}
	}
	return results, nil
}

func (b *EmbeddingBatcher) embedDirect(ctx context.Context, ln *embedLane, texts []string, model string) ([]EmbedResult, error) {
	out := make([]EmbedResult, 0, len(texts))
	for start := 0; start < len(texts); start += ln.maxBatch {
		end := min(start+ln.maxBatch, len(texts))
		res, err := b.callLane(ctx, ln, texts[start:end], model)
		if err != nil {
			return nil, err
		}
		out = append(out, res...)
	}
	return out, nil
}

func (b *EmbeddingBatcher) enqueue(ln *embedLane, req EmbedRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		// ResultCh 可能由外部调用方构造为无缓冲/已满，持锁发送不得阻塞（L-05）
		select {
		case req.ResultCh <- EmbedResult{Error: ErrBatcherStopped}:
		default:
		}
		return
	}

	// 去重：同 text 已在本通道排队 → 追加到扇出列表，不再占用队列槽位。
	key := textHash(req.Text)
	if _, exists := ln.dedup[key]; exists {
		ln.dedup[key] = append(ln.dedup[key], req.ResultCh)
		return
	}
	if len(ln.pending) >= ln.capacity {
		select {
		case req.ResultCh <- EmbedResult{Error: ErrBatcherSaturated}:
		default:
		}
		return
	}
	ln.dedup[key] = []chan EmbedResult{req.ResultCh}
	ln.pending = append(ln.pending, req)
}

func (b *EmbeddingBatcher) flushBatch(ctx context.Context, texts []string, model string) ([]EmbedResult, error) {
	if b.embedFn == nil {
		return nil, apperr.New(apperr.CodeInternal, "embedding batcher: embedFn not configured")
	}
	vecs, err := b.embedFn(ctx, texts, model)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "embedding batch call failed", err)
	}
	results := make([]EmbedResult, len(texts))
	for i, vec := range vecs {
		if i < len(results) {
			results[i] = EmbedResult{Vector: vec}
		}
	}
	return results, nil
}
