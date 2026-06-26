package llm

import (
	"context"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/store/search"
)

// DynamicEmbedder 是一个线程安全的 Embedder 代理。
// 允许在系统运行期间（如后台 Ollama 下载完成后）动态无缝替换底层向量化引擎，
// 而不需要加锁，彻底解决替换期间各个 Handler 访问的 Data Race 问题。
type DynamicEmbedder struct {
	ptr     atomic.Pointer[search.Embedder]
	readyCh chan struct{}
}

func NewDynamicEmbedder() *DynamicEmbedder {
	return &DynamicEmbedder{
		readyCh: make(chan struct{}),
	}
}

// Set 原子替换底层的 Embedder 实例。
func (d *DynamicEmbedder) Set(e search.Embedder) {
	if e == nil {
		d.ptr.Store(nil)
		return
	}
	d.ptr.Store(&e)

	// 触发就绪事件（仅触发一次）
	select {
	case <-d.readyCh:
	default:
		close(d.readyCh)
	}
}

// WaitReady 返回一个 channel，当 Embedder 首次被成功注入时该 channel 会被关闭。
// 用于触发后台回填等异步任务。
func (d *DynamicEmbedder) WaitReady() <-chan struct{} {
	return d.readyCh
}

// Embed 实现 search.Embedder 接口。
func (d *DynamicEmbedder) Embed(text string) []float32 {
	p := d.ptr.Load()
	if p == nil || *p == nil {
		return nil
	}
	return (*p).Embed(text)
}

// EmbedBatch 检查底层引擎是否支持批量操作。如果支持则透传调用，否则自动降级为逐条 Embed。
// 这保证了像 EmbeddingIndexer 这样的组件可以安全地调用 EmbedBatch，
// 而不需要关心底层引擎的具体能力。
func (d *DynamicEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	p := d.ptr.Load()
	if p == nil || *p == nil {
		// 底层暂不可用，返回全 nil，避免 Panic
		res := make([][]float32, len(texts))
		return res, nil
	}

	e := *p
	if be, ok := e.(interface {
		EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	}); ok {
		return be.EmbedBatch(ctx, texts)
	}

	// 逐条降级（不支持批量的普通 Embedder）
	res := make([][]float32, len(texts))
	for i, t := range texts {
		res[i] = e.Embed(t)
	}
	return res, nil
}
