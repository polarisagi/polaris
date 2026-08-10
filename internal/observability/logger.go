package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"gopkg.in/natefinch/lumberjack.v2"
)

// TeeHandler routes log records to multiple handlers.
type TeeHandler struct {
	handlers []slog.Handler
}

// NewTeeHandler creates a handler that duplicates its records to all provided handlers.
func NewTeeHandler(handlers ...slog.Handler) slog.Handler {
	return &TeeHandler{handlers: handlers}
}

// Enabled returns true if any handler is enabled for the level.
func (t *TeeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range t.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle calls Handle on all enabled handlers.
func (t *TeeHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range t.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "TeeHandler.Handle", err)
			}
		}
	}
	return nil
}

// WithAttrs adds attributes to all handlers.
func (t *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(t.handlers))
	for i, h := range t.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &TeeHandler{handlers: handlers}
}

// WithGroup adds a group to all handlers.
func (t *TeeHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(t.handlers))
	for i, h := range t.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &TeeHandler{handlers: handlers}
}

// MultiCloser closes multiple io.Closers.
type MultiCloser struct {
	closers []io.Closer
}

// Close closes all underlying closers and returns the first error encountered.
func (m *MultiCloser) Close() error {
	var firstErr error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// logLevel reads LOG_LEVEL env var, defaulting to Info. Supports debug/info/warn/error.
func logLevel() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// SetupLogger configures rotating dual-track logging.
// It creates polaris.log (all levels) and polaris.error.log (Warn/Error) in dataDir/logs/.
func SetupLogger(dataDir string) io.Closer {
	var closers []io.Closer

	logsDir := filepath.Join(dataDir, "logs")
	// 失败必须可见：logger 尚未建好，只能走 stderr。吞掉的话后续 lumberjack 会在
	// 每次写日志时静默失败，表现为"服务在跑但没有任何日志"——最难排查的一类现象。
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "polaris: 创建日志目录 %s 失败（日志将无法落盘，请检查目录权限）: %v\n", logsDir, err)
	}

	// Main logger
	mainPath := filepath.Join(logsDir, "polaris.log")
	mainWriter := &lumberjack.Logger{
		Filename:   mainPath,
		MaxSize:    50, // megabytes
		MaxBackups: 20,
		MaxAge:     30, // days
		Compress:   true,
	}
	closers = append(closers, mainWriter)

	mainMw := io.Writer(mainWriter)

	mainOpts := &slog.HandlerOptions{
		Level:     logLevel(),
		AddSource: false,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format(time.RFC3339))
			}
			return a
		},
	}
	mainHandler := slog.NewTextHandler(mainMw, mainOpts)

	// Error logger
	errorPath := filepath.Join(logsDir, "polaris.error.log")
	errorWriter := &lumberjack.Logger{
		Filename:   errorPath,
		MaxSize:    50,
		MaxBackups: 20,
		MaxAge:     30,
		Compress:   true,
	}
	closers = append(closers, errorWriter)

	errorOpts := &slog.HandlerOptions{
		Level:     slog.LevelWarn, // Only Warn and Error
		AddSource: false,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format(time.RFC3339))
			}
			return a
		},
	}
	errorHandler := slog.NewTextHandler(errorWriter, errorOpts)

	// Combine handlers
	tee := NewTeeHandler(mainHandler, errorHandler)
	slog.SetDefault(slog.New(tee))

	return &MultiCloser{closers: closers}
}
