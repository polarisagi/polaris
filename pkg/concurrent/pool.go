package concurrent

import (
	"context"
	"log/slog"
)

// SafeGo executes fn in a new goroutine with a panic recovery mechanism.
func SafeGo(ctx context.Context, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic recovered in SafeGo", "name", name, "panic", r)
			}
		}()
		fn()
	}()
}
