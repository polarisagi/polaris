package llm

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
)

// GD-13-002：重载期间并发读者任何时刻都不应看到空注册表。
func TestProviderRegistry_ReplaceAllNoEmptyWindow(t *testing.T) {
	r := NewProviderRegistry(config.M1RouterThresholds{})
	r.RegisterWithRole("old", "old", "default", &mockProvider{})

	var empty atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.mu.RLock()
			n := len(r.entries)
			r.mu.RUnlock()
			if n == 0 {
				empty.Add(1)
			}
		}
	}()
	for i := 0; i < 200; i++ {
		name := "new"
		if i%2 == 1 {
			name = "old"
		}
		r.ReplaceAll(func(register func(string, string, string, protocol.Provider)) {
			register(name, name, "default", &mockProvider{})
		})
	}
	close(stop)
	wg.Wait()
	if empty.Load() != 0 {
		t.Fatalf("readers observed empty registry %d times", empty.Load())
	}
}
