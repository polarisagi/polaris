package audiorun

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type fakeEng struct{ id int32 }

type slotHarness struct {
	loads, unloads atomic.Int32
	loadDelay      time.Duration
	loadErr        error
	events         []string
	mu             sync.Mutex
}

func (h *slotHarness) record(ev string) {
	h.mu.Lock()
	h.events = append(h.events, ev)
	h.mu.Unlock()
}

func (h *slotHarness) eventsCopy() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func (h *slotHarness) slot(idle, wait time.Duration) *Slot[*fakeEng] {
	return NewSlot(SlotConfig[*fakeEng]{
		Name: "test",
		Load: func(ctx context.Context) (*fakeEng, error) {
			time.Sleep(h.loadDelay)
			if h.loadErr != nil {
				return nil, h.loadErr
			}
			return &fakeEng{id: h.loads.Add(1)}, nil
		},
		Unload:     func(*fakeEng) { h.unloads.Add(1) },
		IdleUnload: idle,
		LoadWait:   wait,
		Hooks: SlotHooks{
			OnLoading:    func() { h.record("loading") },
			OnLoaded:     func() { h.record("loaded") },
			OnUnloaded:   func() { h.record("unloaded") },
			OnLoadFailed: func(error) { h.record("failed") },
		},
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// 懒加载：构造时不加载，首次 Acquire 才加载，之后复用同一实例。
func TestSlot_LazyLoadThenReuse(t *testing.T) {
	h := &slotHarness{}
	s := h.slot(0, time.Second)
	defer s.Close()
	if s.IsResident() || h.loads.Load() != 0 {
		t.Fatal("构造时不得加载")
	}
	e1, rel1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel1()
	e2, rel2, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	if e1 != e2 || h.loads.Load() != 1 {
		t.Errorf("应复用同一引擎，loads=%d", h.loads.Load())
	}
}

// 并发请求共享同一次加载，而不是各加载一份（那会让 600MB 的引擎同时驻留 N 份）。
func TestSlot_ConcurrentAcquireSharesOneLoad(t *testing.T) {
	h := &slotHarness{loadDelay: 80 * time.Millisecond}
	s := h.slot(0, 2*time.Second)
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rel, err := s.Acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			rel()
		}()
	}
	wg.Wait()
	if h.loads.Load() != 1 {
		t.Errorf("8 个并发请求应只触发 1 次加载，got %d", h.loads.Load())
	}
}

// 空闲卸载 + 再次请求自动重新加载。
func TestSlot_IdleUnloadThenReload(t *testing.T) {
	h := &slotHarness{}
	s := h.slot(60*time.Millisecond, time.Second)
	defer s.Close()
	_, rel, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel()
	waitFor(t, "空闲卸载", func() bool { return !s.IsResident() })
	if h.unloads.Load() != 1 {
		t.Errorf("unloads=%d, want 1", h.unloads.Load())
	}
	e, rel, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	if e.id != 2 || h.loads.Load() != 2 {
		t.Errorf("卸载后应重新加载一份新引擎，id=%d loads=%d", e.id, h.loads.Load())
	}
	ev := h.eventsCopy()
	want := []string{"loading", "loaded", "unloaded", "loading", "loaded"}
	if len(ev) != len(want) {
		t.Fatalf("事件序列 %v, want %v", ev, want)
	}
	for i := range want {
		if ev[i] != want[i] {
			t.Fatalf("事件序列 %v, want %v", ev, want)
		}
	}
}

// 有请求在用时绝不卸载（否则推理中途 Close = 崩溃）。
func TestSlot_NoUnloadWhileInUse(t *testing.T) {
	h := &slotHarness{}
	s := h.slot(30*time.Millisecond, time.Second)
	defer s.Close()
	_, rel, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 远超空闲阈值
	if !s.IsResident() || h.unloads.Load() != 0 {
		t.Fatal("使用期间不得卸载")
	}
	if s.Unload() {
		t.Error("Unload 在使用中应返回 false")
	}
	rel()
	waitFor(t, "释放后空闲卸载", func() bool { return !s.IsResident() })
}

// idle<=0 = 永不卸载。
func TestSlot_ZeroIdleNeverUnloads(t *testing.T) {
	h := &slotHarness{}
	s := h.slot(0, time.Second)
	defer s.Close()
	_, rel, _ := s.Acquire(context.Background())
	rel()
	time.Sleep(100 * time.Millisecond)
	if !s.IsResident() {
		t.Error("idle=0 不应卸载")
	}
}

// 加载失败：所有等待者拿到同一个错误；失败后可重试成功。
func TestSlot_LoadErrorPropagatesAndRetries(t *testing.T) {
	h := &slotHarness{loadErr: errors.New("boom")}
	s := h.slot(0, time.Second)
	defer s.Close()
	if _, _, err := s.Acquire(context.Background()); err == nil || err.Error() != "boom" {
		t.Fatalf("应返回加载错误，got %v", err)
	}
	h.loadErr = nil
	_, rel, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("失败后重试应成功: %v", err)
	}
	rel()
}

// 等待超时只放弃等待，后台加载继续：稍后重试命中已加载引擎。
func TestSlot_LoadWaitTimeoutKeepsLoading(t *testing.T) {
	h := &slotHarness{loadDelay: 200 * time.Millisecond}
	s := h.slot(0, 30*time.Millisecond)
	defer s.Close()
	_, _, err := s.Acquire(context.Background())
	nr, ok := AsNotReady(err)
	if !ok || nr.Code != CodeLoadTimeout {
		t.Fatalf("应报 loading_timeout，got %v", err)
	}
	waitFor(t, "后台加载完成", s.IsResident)
	_, rel, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("加载完成后应直接命中: %v", err)
	}
	rel()
	if h.loads.Load() != 1 {
		t.Errorf("超时不应触发第二次加载，loads=%d", h.loads.Load())
	}
}

func TestSlot_ContextCancelled(t *testing.T) {
	h := &slotHarness{loadDelay: 300 * time.Millisecond}
	s := h.slot(0, 5*time.Second)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := s.Acquire(ctx)
	if !apperr.IsCode(err, apperr.CodeCancelled) {
		t.Errorf("ctx 取消应返回 CodeCancelled，got %v", err)
	}
}

func TestSlot_CloseUnloadsAndRejects(t *testing.T) {
	h := &slotHarness{}
	s := h.slot(0, time.Second)
	_, rel, _ := s.Acquire(context.Background())
	rel()
	s.Close()
	if h.unloads.Load() != 1 || s.IsResident() {
		t.Error("Close 应卸载引擎")
	}
	if _, _, err := s.Acquire(context.Background()); err == nil {
		t.Error("Close 后 Acquire 应报错")
	}
}

// load 内 panic 不得让等待者干等到超时。
func TestSlot_LoadPanicDoesNotHangWaiters(t *testing.T) {
	s := NewSlot(SlotConfig[*fakeEng]{
		Name:     "panic",
		Load:     func(context.Context) (*fakeEng, error) { panic("kaboom") },
		LoadWait: 2 * time.Second,
	})
	defer s.Close()
	start := time.Now()
	_, _, err := s.Acquire(context.Background())
	if err == nil {
		t.Fatal("panic 应转为错误")
	}
	if time.Since(start) > time.Second {
		t.Errorf("等待者被拖到了超时，耗时 %v", time.Since(start))
	}
}

// release 幂等：重复调用不得把 inflight 减成负数而提前卸载。
func TestSlot_ReleaseIdempotent(t *testing.T) {
	h := &slotHarness{}
	s := h.slot(0, time.Second)
	defer s.Close()
	_, rel1, _ := s.Acquire(context.Background())
	_, rel2, _ := s.Acquire(context.Background())
	rel1()
	rel1() // 重复
	if s.Unload() {
		t.Error("仍有一个使用者，不得卸载")
	}
	rel2()
	if !s.Unload() {
		t.Error("全部释放后应可卸载")
	}
}
