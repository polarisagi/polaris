package audiorun

import (
	"context"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// DefaultLoadWait 是请求等待引擎加载完成的上限（audio-v2-spec §2.4：超时 30s）。
// 超时只放弃"等待"，后台加载继续，稍后重试通常即可命中。
const DefaultLoadWait = 30 * time.Second

// loadBudget 是单次加载（含 TTS 首次基准）的总预算，与请求等待上限解耦。
const loadBudget = 5 * time.Minute

// SlotHooks 是 Slot 状态变化的回调；全部可为 nil，且都在 Slot 锁外调用。
type SlotHooks struct {
	OnLoading    func()
	OnLoaded     func()
	OnUnloaded   func()
	OnLoadFailed func(err error)
}

// SlotConfig 配置一个 Slot。
type SlotConfig[E any] struct {
	Name string
	// Load 构造引擎（含内存检查、库加载等）；由后台 goroutine 执行，可能耗时数秒。
	Load func(ctx context.Context) (E, error)
	// Unload 释放引擎（如 Close）；在 Slot 锁内调用，保证与 Acquire 互斥。
	Unload func(E)
	// IdleUnload 引擎最后一次使用后空闲多久卸载；<=0 不卸载。
	IdleUnload time.Duration
	// LoadWait 请求等待加载的上限；<=0 取 DefaultLoadWait。
	LoadWait time.Duration
	Hooks    SlotHooks
}

// loadCall 代表一次进行中的加载；多个并发请求共享同一次加载。
type loadCall struct {
	done chan struct{}
	err  error
}

// Slot 持有一个按需加载、空闲卸载的引擎。
//
// 为什么需要它：STT≈420MB + Kokoro≈600MB 的 RSS 不应在桌面端与用户其他应用抢内存、
// 也不应占用 2GB VPS 的核心预算，所以引擎首次使用才加载、空闲 N 分钟后释放（ADR-0107）。
//
// 并发契约：Acquire 返回的引擎在 release 被调用前不会被卸载（inflight 计数），
// 因此推理中途不会遇到 Close；卸载在锁内进行，与新一轮加载互斥，避免新旧两份引擎同时驻留。
type Slot[E any] struct {
	cfg SlotConfig[E]

	mu       sync.Mutex
	eng      E
	loaded   bool
	call     *loadCall
	inflight int
	lastUsed time.Time
	timer    *time.Timer
	closed   bool
}

// NewSlot 构造 Slot。
func NewSlot[E any](cfg SlotConfig[E]) *Slot[E] {
	if cfg.LoadWait <= 0 {
		cfg.LoadWait = DefaultLoadWait
	}
	return &Slot[E]{cfg: cfg}
}

// IsResident 报告引擎当前是否驻留内存。
func (s *Slot[E]) IsResident() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded
}

// Acquire 取得引擎：已加载则立即返回，否则触发（或加入进行中的）加载并等待至多 LoadWait。
// 调用方必须在用完后调用返回的 release。
func (s *Slot[E]) Acquire(ctx context.Context) (E, func(), error) {
	var zero E
	wait := time.NewTimer(s.cfg.LoadWait)
	defer wait.Stop()

	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return zero, nil, apperr.New(apperr.CodeCancelled, "audiorun: "+s.cfg.Name+" 已关闭")
		}
		if s.loaded {
			s.inflight++
			s.disarmLocked()
			e := s.eng
			s.mu.Unlock()
			return e, s.releaser(), nil
		}
		c := s.call
		if c == nil {
			c = &loadCall{done: make(chan struct{})}
			s.call = c
			s.startLoad(c)
		}
		s.mu.Unlock()

		select {
		case <-c.done:
			if c.err != nil {
				return zero, nil, c.err
			}
			// 加载成功：回到循环顶部取引擎（期间若被卸载则自然触发重新加载）。
		case <-wait.C:
			return zero, nil, notReady(CodeLoadTimeout, "语音引擎仍在加载中，请稍后重试（后台加载继续进行）")
		case <-ctx.Done():
			return zero, nil, apperr.Wrap(apperr.CodeCancelled, "audiorun: 等待 "+s.cfg.Name+" 加载时请求被取消", ctx.Err())
		}
	}
}

// releaser 返回幂等的 release：减 inflight，归零后以最后使用时刻为基准重新计空闲时钟。
func (s *Slot[E]) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.inflight--
			s.lastUsed = time.Now()
			if s.inflight == 0 {
				s.armLocked(s.cfg.IdleUnload)
			}
		})
	}
}

// startLoad 在后台 goroutine 里执行加载并唤醒所有等待者。调用方持有 s.mu。
func (s *Slot[E]) startLoad(c *loadCall) {
	if s.cfg.Hooks.OnLoading != nil {
		// 回调在锁外调用会更干净，但"进入 loading"必须先于等待者可见；回调本身只做状态发布，
		// 不会回调 Slot，持锁调用不会死锁。
		s.cfg.Hooks.OnLoading()
	}
	concurrent.SafeGo(context.Background(), "audiorun.slot_load."+s.cfg.Name, func(ctx context.Context) {
		lctx, cancel := context.WithTimeout(ctx, loadBudget)
		defer cancel()

		var (
			e   E
			err error
			ok  bool
		)
		// 兜底：load 内 panic 时也必须结束本次加载，否则所有等待者只能干等到超时。
		defer func() {
			if !ok {
				s.finishLoad(c, e, apperr.New(apperr.CodeInternal, "audiorun: "+s.cfg.Name+" 加载异常终止"))
			}
		}()
		e, err = s.cfg.Load(lctx)
		ok = true
		s.finishLoad(c, e, err)
	})
}

// finishLoad 登记加载结果、唤醒等待者并发出状态回调。
func (s *Slot[E]) finishLoad(c *loadCall, e E, err error) {
	s.mu.Lock()
	if s.call == c {
		s.call = nil
	}
	discard := false
	switch {
	case err != nil:
	case s.closed:
		discard = true
		err = apperr.New(apperr.CodeCancelled, "audiorun: "+s.cfg.Name+" 加载完成时已关闭")
	default:
		s.eng, s.loaded = e, true
		s.lastUsed = time.Now()
		s.armLocked(s.cfg.IdleUnload) // 加载后若无人取用，也要按空闲计时卸载
	}
	c.err = err
	s.mu.Unlock()

	if discard && s.cfg.Unload != nil {
		s.cfg.Unload(e)
	}
	switch {
	case err != nil && s.cfg.Hooks.OnLoadFailed != nil:
		s.cfg.Hooks.OnLoadFailed(err)
	case err == nil && s.cfg.Hooks.OnLoaded != nil:
		s.cfg.Hooks.OnLoaded()
	}
	// 先发状态、后唤醒等待者：请求拿到 503 时，capabilities 里的状态必然已同步更新，
	// 前端据错误刷新状态不会读到过期的 "loading"。
	close(c.done)
}

// armLocked 重置空闲卸载定时器；d<=0 表示不卸载。调用方持有 s.mu。
func (s *Slot[E]) armLocked(d time.Duration) {
	s.disarmLocked()
	if d <= 0 || !s.loaded {
		return
	}
	s.timer = time.AfterFunc(d, s.onIdle)
}

func (s *Slot[E]) disarmLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// onIdle 是空闲定时器回调：仅当无人使用且确已空闲足够久才卸载，否则按剩余时间重排。
func (s *Slot[E]) onIdle() {
	s.mu.Lock()
	if !s.loaded || s.inflight > 0 {
		s.mu.Unlock()
		return
	}
	if rest := s.cfg.IdleUnload - time.Since(s.lastUsed); rest > 0 {
		s.timer = time.AfterFunc(rest, s.onIdle)
		s.mu.Unlock()
		return
	}
	s.unloadLocked()
	s.mu.Unlock()
	if s.cfg.Hooks.OnUnloaded != nil {
		s.cfg.Hooks.OnUnloaded()
	}
}

// unloadLocked 释放引擎。调用方持有 s.mu 且已确认 inflight==0。
func (s *Slot[E]) unloadLocked() {
	var zero E
	e := s.eng
	s.eng, s.loaded = zero, false
	s.disarmLocked()
	if s.cfg.Unload != nil {
		s.cfg.Unload(e)
	}
}

// Unload 立即卸载引擎；有请求在用或未加载时返回 false（不做任何事）。
func (s *Slot[E]) Unload() bool {
	s.mu.Lock()
	if !s.loaded || s.inflight > 0 {
		s.mu.Unlock()
		return false
	}
	s.unloadLocked()
	s.mu.Unlock()
	if s.cfg.Hooks.OnUnloaded != nil {
		s.cfg.Hooks.OnUnloaded()
	}
	return true
}

// Close 永久关闭 Slot：卸载引擎（若无人使用），此后 Acquire 一律报错。
// 有请求在用时引擎保留到其 release 之后由调用方进程退出回收——关停路径不为等推理而阻塞。
func (s *Slot[E]) Close() {
	s.mu.Lock()
	s.closed = true
	if s.loaded && s.inflight == 0 {
		s.unloadLocked()
	}
	s.disarmLocked()
	s.mu.Unlock()
}
