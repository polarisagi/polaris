package concurrent

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync/atomic"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Global panic counter for observability (polaris_goroutine_panic_total)
var PanicTotal atomic.Int64

var onPanic atomic.Pointer[func()]

// SetOnPanic injects a hook to be called when a panic is recovered (e.g. for metrics).
func SetOnPanic(f func()) {
	onPanic.Store(&f)
}

// SafeGo executes fn in a new goroutine with a panic recovery mechanism.
func SafeGo(ctx context.Context, name string, fn func(ctx context.Context)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				PanicTotal.Add(1)
				if p := onPanic.Load(); p != nil {
					(*p)()
				}
				slog.Error("concurrent: goroutine panic recovered",
					"name", name,
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()
		fn(ctx)
	}()
}

// BoundedPool provides a concurrency-limited execution pool.
type BoundedPool struct {
	sem  chan struct{}
	name string
}

// NewBoundedPool creates a new BoundedPool.
func NewBoundedPool(name string, maxConcurrent int) *BoundedPool {
	return &BoundedPool{
		sem:  make(chan struct{}, maxConcurrent),
		name: name,
	}
}

// Submit executes fn in a SafeGo goroutine, blocking if the pool is full until ctx is done.
// If it cannot acquire a permit, it returns CodeResourceExhausted.
func (p *BoundedPool) Submit(ctx context.Context, fn func()) error {
	select {
	case p.sem <- struct{}{}:
		SafeGo(ctx, p.name, func(context.Context) {
			defer func() { <-p.sem }()
			fn()
		})
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return apperr.New(apperr.CodeResourceExhausted, "concurrent: pool full")
	}
}
