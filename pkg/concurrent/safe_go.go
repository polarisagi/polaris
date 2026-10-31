package concurrent

import (
	"context"
	"log/slog"
	"reflect"
	"runtime/debug"
	"sync/atomic"
)

// Global panic counter for observability (polaris_goroutine_panic_total)
var PanicTotal atomic.Int64

var onPanic atomic.Pointer[func()]

// SetOnPanic injects a hook to be called when a panic is recovered (e.g. for metrics).
func SetOnPanic(f func()) {
	if f == nil {
		onPanic.Store(nil)
		return
	}
	onPanic.Store(&f)
}

// SafeGo executes fn in a new goroutine with a panic recovery mechanism.
func SafeGo(ctx context.Context, name string, fn func(ctx context.Context)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				PanicTotal.Add(1)
				panicType := reflect.TypeOf(r).String()
				if p := onPanic.Load(); p != nil {
					invokePanicObserver(ctx, name, p)
				}
				slog.ErrorContext(ctx, "concurrent: goroutine panic recovered",
					"name", name,
					"panic_type", panicType,
					"stack", string(debug.Stack()))
			}
		}()
		fn(ctx)
	}()
}

func invokePanicObserver(ctx context.Context, name string, observer *func()) {
	defer func() {
		if callbackPanic := recover(); callbackPanic != nil {
			slog.ErrorContext(ctx, "concurrent: panic observer failed",
				"operation", "on_panic_callback",
				"name", name,
				"callback_panic_type", reflect.TypeOf(callbackPanic).String(),
				"stack", string(debug.Stack()))
		}
	}()
	(*observer)()
}
