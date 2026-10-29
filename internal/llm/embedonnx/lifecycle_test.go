package embedonnx

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedEngineLazyLoadingAndConcurrentCalls(t *testing.T) {
	var loadCount int32

	loader := func(ctx context.Context) (*EmbedEngine, error) {
		atomic.AddInt32(&loadCount, 1)
		time.Sleep(20 * time.Millisecond) // 模拟加载耗时
		return &EmbedEngine{
			isGemma: false,
			version: ModelVersionBGE,
		}, nil
	}

	engine := NewManagedEngineWithTimeout(ModelVersionBGE, loader, 100*time.Millisecond)
	defer engine.Close()

	if engine.IsResident() {
		t.Fatal("expected engine not to be resident before first call")
	}

	// 并发 10 个请求同时调用
	const numGoroutines = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			eng, release, err := engine.acquire(context.Background())
			if err != nil {
				t.Errorf("acquire failed: %v", err)
				return
			}
			if eng == nil {
				t.Errorf("expected non-nil engine")
				return
			}
			time.Sleep(10 * time.Millisecond)
			release()
		}()
	}

	wg.Wait()

	if got := atomic.LoadInt32(&loadCount); got != 1 {
		t.Fatalf("expected loader to be called exactly 1 time, got %d", got)
	}

	if !engine.IsResident() {
		t.Fatal("expected engine to be resident while recently used")
	}
}

func TestManagedEngineIdleUnloadAndReload(t *testing.T) {
	var loadCount int32
	idleTimeout := 50 * time.Millisecond

	loader := func(ctx context.Context) (*EmbedEngine, error) {
		atomic.AddInt32(&loadCount, 1)
		return &EmbedEngine{
			isGemma: false,
			version: ModelVersionBGE,
		}, nil
	}

	engine := NewManagedEngineWithTimeout(ModelVersionBGE, loader, idleTimeout)
	defer engine.Close()

	// 首次 acquire
	eng, release, err := engine.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if eng == nil {
		t.Fatal("expected non-nil engine")
	}
	if !engine.IsResident() {
		t.Fatal("expected resident")
	}
	release()

	// 等待空闲超时触发卸载
	time.Sleep(100 * time.Millisecond)

	if engine.IsResident() {
		t.Fatal("expected engine to be unloaded after idle timeout")
	}

	// 再次 acquire，应当透明重载
	eng2, release2, err := engine.acquire(context.Background())
	if err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}
	if eng2 == nil {
		t.Fatal("expected non-nil engine after reload")
	}
	if !engine.IsResident() {
		t.Fatal("expected resident after reload")
	}
	release2()

	if got := atomic.LoadInt32(&loadCount); got != 2 {
		t.Fatalf("expected loader to be called exactly 2 times after reload, got %d", got)
	}
}
