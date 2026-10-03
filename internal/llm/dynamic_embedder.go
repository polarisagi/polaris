package llm

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// DynamicEmbedder 是一个线程安全的 Embedder 代理。
// 允许在系统运行期间（如后台 Ollama 下载完成后）动态无缝替换底层向量化引擎，
// 而不需要加锁，彻底解决替换期间各个 Handler 访问的 Data Race 问题。
type DynamicEmbedder struct {
	ptr       atomic.Pointer[search.Embedder]
	version   atomic.Pointer[string]
	readyCh   chan struct{}
	readyOnce sync.Once
	mu        sync.Mutex
	onSet     []func()
}

func NewDynamicEmbedder() *DynamicEmbedder {
	return &DynamicEmbedder{
		readyCh: make(chan struct{}),
	}
}

// OnSet 注册当底层 Embedder 被 Set 时调用的回调函数（例如重置退避器、检测模型切换）。
// 回调执行时 ModelVersion() 已是新引擎的版本。
func (d *DynamicEmbedder) OnSet(fn func()) {
	if fn == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onSet = append(d.onSet, fn)
}

// Set 原子替换底层的 Embedder 实例；模型版本取自引擎自身的 ModelVersion()（若实现）。
func (d *DynamicEmbedder) Set(e search.Embedder) {
	version := ""
	if v, ok := e.(interface{ ModelVersion() string }); ok && e != nil {
		version = v.ModelVersion()
	}
	d.SetWithVersion(e, version)
}

// SetWithVersion 原子替换底层 Embedder，并显式记录其模型版本标识。
//
// 模型版本是 embed_model_version（重嵌判据）与"同维换模型清空向量库"的唯一来源
// （ADR-0109 P4）：不同模型的向量即使维度相同也不在同一语义空间，不得混检。
// 远程 / Ollama 适配器没有 ModelVersion()，由装配方传入 "remote:<model>" / "ollama:<model>"。
func (d *DynamicEmbedder) SetWithVersion(e search.Embedder, version string) {
	if e == nil {
		d.ptr.Store(nil)
		d.version.Store(nil)
		return
	}
	d.version.Store(&version)
	d.ptr.Store(&e)

	// 触发就绪事件（仅一次；sync.Once 防止并发 Set 重复 close 导致 panic）
	d.readyOnce.Do(func() { close(d.readyCh) })

	d.mu.Lock()
	callbacks := make([]func(), len(d.onSet))
	copy(callbacks, d.onSet)
	d.mu.Unlock()
	for _, cb := range callbacks {
		cb()
	}
}

// ModelVersion 返回当前引擎的模型版本标识；尚无引擎时返回 ""（调用方据此判定未就绪）。
func (d *DynamicEmbedder) ModelVersion() string {
	if p := d.version.Load(); p != nil {
		return *p
	}
	return ""
}

// WaitReady 返回一个 channel，当 Embedder 首次被成功注入时该 channel 会被关闭。
// 用于触发后台回填等异步任务。
func (d *DynamicEmbedder) WaitReady() <-chan struct{} {
	return d.readyCh
}

// Embed 实现 search.Embedder 接口。
func (d *DynamicEmbedder) Embed(ctx context.Context, text string) []float32 {
	p := d.ptr.Load()
	if p == nil || *p == nil {
		return nil
	}
	return (*p).Embed(ctx, text)
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
		res, err := be.EmbedBatch(ctx, texts)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "dynamic_embedder: EmbedBatch 失败", err)
		}
		return res, nil
	}

	// 逐条降级（不支持批量的普通 Embedder）
	res := make([][]float32, len(texts))
	for i, t := range texts {
		res[i] = e.Embed(ctx, t)
	}
	return res, nil
}
