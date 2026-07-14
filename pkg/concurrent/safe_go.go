package concurrent

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
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

// 2026-07-14（ADR-0051）：BoundedPool/NewBoundedPool/Submit 删除——全仓零调用点、
// 零测试覆盖、docs/ 内零引用，是完全孤立的投机性并发原语，无任何计划中的消费方。
