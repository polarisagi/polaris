package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"gopkg.in/natefinch/lumberjack.v2"
)

// TeeHandler 避免单个落盘通道故障截断其余通道。
type TeeHandler struct {
	handlers []slog.Handler
	fallback slog.Handler
}

// NewTeeHandler 在磁盘通道失败时保留独立 stderr 出口。
func NewTeeHandler(handlers ...slog.Handler) slog.Handler {
	return &TeeHandler{
		handlers: handlers,
		fallback: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
}

// Enabled 保留所有通道共同的级别过滤语义。
func (t *TeeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range t.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle 先尝试全部通道，磁盘失败时直接写 stderr，不回调全局 logger。
func (t *TeeHandler) Handle(ctx context.Context, r slog.Record) error {
	var sinkErr error
	for _, h := range t.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				sinkErr = errors.Join(sinkErr, apperr.Wrap(apperr.CodeInternal, "TeeHandler.Handle sink failed", err))
			}
		}
	}
	if sinkErr == nil || t.fallback == nil || !t.fallback.Enabled(ctx, r.Level) {
		if sinkErr != nil {
			return apperr.Wrap(apperr.CodeInternal, "TeeHandler.Handle failed", sinkErr)
		}
		return nil
	}
	if err := t.fallback.Handle(ctx, r.Clone()); err != nil {
		joined := errors.Join(sinkErr, apperr.Wrap(apperr.CodeInternal, "TeeHandler.Handle stderr fallback failed", err))
		return apperr.Wrap(apperr.CodeInternal, "TeeHandler.Handle fallback failed", joined)
	}
	return apperr.Wrap(apperr.CodeInternal, "TeeHandler.Handle failed", sinkErr)
}

// WithAttrs 保留子 logger 的 stderr 兜底。
func (t *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(t.handlers))
	for i, h := range t.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	var fallback slog.Handler
	if t.fallback != nil {
		fallback = t.fallback.WithAttrs(attrs)
	}
	return &TeeHandler{handlers: handlers, fallback: fallback}
}

// WithGroup 保留子 logger 的 stderr 兜底。
func (t *TeeHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(t.handlers))
	for i, h := range t.handlers {
		handlers[i] = h.WithGroup(name)
	}
	var fallback slog.Handler
	if t.fallback != nil {
		fallback = t.fallback.WithGroup(name)
	}
	return &TeeHandler{handlers: handlers, fallback: fallback}
}

// MultiCloser 集中关闭轮转日志文件。
type MultiCloser struct {
	closers []io.Closer
}

// Close 尽力关闭所有文件并返回首个错误。
func (m *MultiCloser) Close() error {
	var firstErr error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// logLevel 让启动配置控制默认日志级别。
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

// secureLogPath 收紧已有日志文件；拒绝符号链接以免 chmod 跟随到受控目录外。
func secureLogPath(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "inspect log file", err)
	}
	if !info.Mode().IsRegular() {
		return apperr.New(apperr.CodeInvalidInput, "log path is not a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "secure log file permissions", err)
	}
	return nil
}

// SetupLogger 在受控目录安装双轨轮转日志；权限无法收紧时切换到 stderr。
func SetupLogger(dir string) io.Closer {
	logsDir := dir
	if filepath.Base(dir) != "logs" {
		logsDir = filepath.Join(dir, "logs")
	}
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "polaris: create log directory failed; file logging disabled: %v\n", err)
		return installStderrLogger()
	}
	info, err := os.Lstat(logsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "polaris: inspect log directory failed; file logging disabled: %v\n", err)
		return installStderrLogger()
	}
	if !info.IsDir() {
		fmt.Fprintln(os.Stderr, "polaris: log path is not a directory; file logging disabled")
		return installStderrLogger()
	}
	if err := os.Chmod(logsDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "polaris: secure log directory failed; file logging disabled: %v\n", err)
		return installStderrLogger()
	}

	mainPath := filepath.Join(logsDir, "polaris.log")
	errorPath := filepath.Join(logsDir, "polaris.error.log")
	for _, path := range []string{mainPath, errorPath} {
		if err := secureLogPath(path); err != nil {
			fmt.Fprintf(os.Stderr, "polaris: secure log file failed; file logging disabled: %v\n", err)
			return installStderrLogger()
		}
	}

	mainWriter := &lumberjack.Logger{
		Filename:   mainPath,
		MaxSize:    50,
		MaxBackups: 20,
		MaxAge:     30,
		Compress:   true,
	}
	errorWriter := &lumberjack.Logger{
		Filename:   errorPath,
		MaxSize:    50,
		MaxBackups: 20,
		MaxAge:     30,
		Compress:   true,
	}
	mainOpts := &slog.HandlerOptions{
		Level: logLevel(),
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format(time.RFC3339))
			}
			return a
		},
	}
	errorOpts := &slog.HandlerOptions{
		Level: slog.LevelWarn,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format(time.RFC3339))
			}
			return a
		},
	}
	tee := NewTeeHandler(slog.NewTextHandler(mainWriter, mainOpts), slog.NewTextHandler(errorWriter, errorOpts))
	slog.SetDefault(slog.New(tee))
	return &MultiCloser{closers: []io.Closer{mainWriter, errorWriter}}
}

func installStderrLogger() io.Closer {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
	return &MultiCloser{}
}
