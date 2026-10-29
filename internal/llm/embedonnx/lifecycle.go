package embedonnx

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// DefaultIdleUnloadTimeout 是模型空闲卸载的默认超时时间（10 分钟）。
// 理由：向量化在交互会话中频度较高（每轮对话相隔几秒至数十秒），10 分钟窗口既能避免
// 连续对话期间频繁创建/销毁 session 导致的延迟颠簸，又能在会话彻底空闲后释放原生内存（ADR-0109）。
const DefaultIdleUnloadTimeout = 10 * time.Minute

// EngineLoader 构造底层 EmbedEngine 的加载函数。
type EngineLoader func(ctx context.Context) (*EmbedEngine, error)

// ManagedEngine 包装 EmbedEngine，提供懒加载与空闲 10 分钟卸载能力。
type ManagedEngine struct {
	loader      EngineLoader
	idleTimeout time.Duration
	version     string

	mu       sync.Mutex
	cond     *sync.Cond
	engine   *EmbedEngine
	loading  bool
	loadErr  error
	inflight int
	timer    *time.Timer
	closed   bool
}

// NewManagedEngine 构造受生命周期管理的向量引擎。
func NewManagedEngine(version string, loader EngineLoader) *ManagedEngine {
	return NewManagedEngineWithTimeout(version, loader, DefaultIdleUnloadTimeout)
}

// NewManagedEngineWithTimeout 构造带自定义空闲超时时间的引擎（供单测使用）。
func NewManagedEngineWithTimeout(version string, loader EngineLoader, idleTimeout time.Duration) *ManagedEngine {
	m := &ManagedEngine{
		loader:      loader,
		idleTimeout: idleTimeout,
		version:     version,
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// IsResident 报告底层 ONNX Session 当前是否驻留内存。
func (m *ManagedEngine) IsResident() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.engine != nil
}

// ModelVersion 返回模型版本。
func (m *ManagedEngine) ModelVersion() string {
	return m.version
}

// Dim 返回维度（固定 512）。
func (m *ManagedEngine) Dim() int {
	return 512
}

// acquire 获取引擎实例，若未加载则触发加载。
// 调用方使用完后必须调用返回的 release 函数。
func (m *ManagedEngine) acquire(ctx context.Context) (*EmbedEngine, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for {
		if m.closed {
			return nil, nil, apperr.New(apperr.CodeCancelled, "embedonnx: managed engine is closed")
		}

		if m.engine != nil {
			if m.timer != nil {
				m.timer.Stop()
				m.timer = nil
			}
			m.inflight++
			release := func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				m.inflight--
				if m.inflight == 0 && m.engine != nil && !m.closed && m.idleTimeout > 0 {
					if m.timer != nil {
						m.timer.Stop()
					}
					m.timer = time.AfterFunc(m.idleTimeout, m.onIdleTimeout)
				}
			}
			return m.engine, release, nil
		}

		if m.loading {
			m.cond.Wait()
			continue
		}

		// 触发加载（仅首个请求执行加载）
		m.loading = true
		m.mu.Unlock()

		eng, err := m.loader(ctx)

		m.mu.Lock()
		m.loading = false
		if err != nil {
			m.loadErr = err
			m.cond.Broadcast()
			return nil, nil, err
		}

		m.engine = eng
		m.loadErr = nil
		m.cond.Broadcast()
		slog.Info("embedonnx: lazy loaded ONNX embedding session", "version", m.version)
	}
}

func (m *ManagedEngine) onIdleTimeout() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.inflight > 0 || m.engine == nil || m.closed {
		return
	}

	slog.Info("embedonnx: 10m idle timeout reached, unloading ONNX session to free memory", "version", m.version)
	m.engine.Close()
	m.engine = nil
	m.timer = nil
}

// Embed 实现 search.Embedder 接口。
func (m *ManagedEngine) Embed(ctx context.Context, text string) []float32 {
	eng, release, err := m.acquire(ctx)
	if err != nil {
		slog.Warn("embedonnx: acquire engine failed", "err", err)
		return nil
	}
	defer release()
	return eng.Embed(ctx, text)
}

// EmbedBatch 批量计算向量。
func (m *ManagedEngine) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	eng, release, err := m.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return eng.EmbedBatch(ctx, texts)
}

// Close 关闭引擎并停止定时器。
func (m *ManagedEngine) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.engine != nil {
		m.engine.Close()
		m.engine = nil
	}
	m.cond.Broadcast()
}
