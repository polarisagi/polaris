package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── LogEntry ───────────────────────────────────────────────────────────────────

// LogEntry 单条日志记录（JSON 序列化后推 SSE）。
type LogEntry struct {
	Time    string         `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// ─── LogStore → slog.Handler ────────────────────────────────────────────────────

// logStoreShared 是环形缓冲 + SSE 订阅表的共享状态，被同一颗 slog 调用树上
// 所有由 WithAttrs/WithGroup 派生出的 *LogStore 实例共享同一份（ADR-0094 决策七）。
// 拆出这一层是因为 *LogStore 本身不能被值拷贝（内嵌 sync.RWMutex），而
// WithAttrs/WithGroup 又必须返回一个新的 *LogStore 实例（不能像修复前那样直接
// 返回 s.wrapped.WithAttrs(attrs) 剥离掉 LogStore 包装）——用指针间接层让多个
// *LogStore 实例安全共享同一把锁和同一个环形缓冲/订阅表。
type logStoreShared struct {
	mu        sync.RWMutex
	ring      []LogEntry // 环形缓冲
	ringCap   int
	ringHead  int
	ringCount int
	nextSubID int
	subs      map[int]chan LogEntry
}

// LogStore 实现 slog.Handler，同时持有环形缓冲 + SSE 广播扇出。
// 创建后通过 slog.SetDefault(slog.New(store)) 挂载，所有 slog.* 调用均经过此处。
type LogStore struct {
	shared  *logStoreShared
	wrapped slog.Handler // 下游 handler（文件 + stdout），WithAttrs/WithGroup 时替换
}

// NewLogStore 创建 LogStore。ringCap=0 默认 500 条。
func NewLogStore(wrapped slog.Handler, ringCap int) *LogStore {
	if ringCap <= 0 {
		ringCap = 500
	}
	return &LogStore{
		shared: &logStoreShared{
			ring:    make([]LogEntry, ringCap),
			ringCap: ringCap,
			subs:    make(map[int]chan LogEntry),
		},
		wrapped: wrapped,
	}
}

// ── slog.Handler 接口 ──────────────────────────────────────────────────────────

func (s *LogStore) Enabled(ctx context.Context, l slog.Level) bool {
	return s.wrapped.Enabled(ctx, l)
}

func (s *LogStore) Handle(ctx context.Context, r slog.Record) error {
	entry := LogEntry{
		Time:    r.Time.Format(time.RFC3339),
		Level:   strings.ToLower(r.Level.String()),
		Message: r.Message,
	}
	if r.NumAttrs() > 0 {
		entry.Attrs = make(map[string]any)
		r.Attrs(func(a slog.Attr) bool {
			val := a.Value.Any()
			if err, ok := val.(error); ok {
				entry.Attrs[a.Key] = err.Error()
			} else {
				entry.Attrs[a.Key] = val
			}
			return true
		})
	}

	// 写入环形缓冲
	s.shared.mu.Lock()
	s.shared.ring[s.shared.ringHead] = entry
	s.shared.ringHead = (s.shared.ringHead + 1) % s.shared.ringCap
	if s.shared.ringCount < s.shared.ringCap {
		s.shared.ringCount++
	}

	// 广播给所有 SSE 订阅者（非阻塞丢弃）
	for id, ch := range s.shared.subs {
		select {
		case ch <- entry:
		default:
			// 订阅者消费太慢，跳过该条
			close(ch)
			delete(s.shared.subs, id)
		}
	}
	s.shared.mu.Unlock()

	if err := s.wrapped.Handle(ctx, r); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "logstream: 底层 Handler.Handle 失败", err)
	}
	return nil
}

// WithAttrs 返回携带附加属性的子 Handler。ADR-0094 决策七：必须返回重新包装了
// LogStore 的新实例（共享同一份 shared 环形缓冲/订阅表），而不是像修复前那样
// 直接返回 s.wrapped.WithAttrs(attrs) 剥离 LogStore——否则全仓库广泛使用的
// logger.With(...) 产生的子 logger 打印的所有日志都会绕过环形缓冲与 SSE 广播，
// 导致这些子模块的日志无法在 /v1/logs/stream 中实时展示。
func (s *LogStore) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogStore{shared: s.shared, wrapped: s.wrapped.WithAttrs(attrs)}
}

// WithGroup 同 WithAttrs，见上方决策七说明。
func (s *LogStore) WithGroup(name string) slog.Handler {
	return &LogStore{shared: s.shared, wrapped: s.wrapped.WithGroup(name)}
}

// ── 订阅/退订 ──────────────────────────────────────────────────────────────────

func (s *LogStore) Recent() []LogEntry {
	s.shared.mu.RLock()
	defer s.shared.mu.RUnlock()

	n := s.shared.ringCount
	out := make([]LogEntry, n)
	if n == 0 {
		return out
	}
	// 环形缓冲线性化：从 ringHead 往前倒着读
	start := (s.shared.ringHead - n + s.shared.ringCap) % s.shared.ringCap
	for i := range n {
		out[i] = s.shared.ring[(start+i)%s.shared.ringCap]
	}
	return out
}

func (s *LogStore) Subscribe() (int, <-chan LogEntry) {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	id := s.shared.nextSubID
	s.shared.nextSubID++
	ch := make(chan LogEntry, 64)
	s.shared.subs[id] = ch
	return id, ch
}

func (s *LogStore) Unsubscribe(id int) {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	if ch, ok := s.shared.subs[id]; ok {
		close(ch)
		delete(s.shared.subs, id)
	}
}

// ─── SSE 端点 ───────────────────────────────────────────────────────────────────

// handleLogStream 提供日志实时 SSE 流。
// 先发 initial batch（最近日志），然后持续推送新日志。
// GET /v1/logs/stream?level=warn（可选过滤最低级别）
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if s.logStore == nil {
		http.Error(w, "log store not available", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	// 可选级别过滤
	minLevel := r.URL.Query().Get("level")

	// 订阅
	subID, ch := s.logStore.Subscribe()
	defer s.logStore.Unsubscribe(subID)

	ctx := r.Context()

	// 发送初始 batch
	recent := s.logStore.Recent()
	for _, entry := range recent {
		if minLevel != "" && !levelGe(entry.Level, minLevel) {
			continue
		}
		data, _ := json.Marshal(entry)
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
	}
	flusher.Flush()

	// 持续推送
	for {
		select {
		case entry, ok := <-ch:
			if !ok {
				return
			}
			if minLevel != "" && !levelGe(entry.Level, minLevel) {
				continue
			}
			data, _ := json.Marshal(entry)
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// levelGe 检查 entry Level 是否 >= minLevel。
// slog 级别顺序: debug < info < warn < error。
func levelGe(level, min string) bool {
	order := map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}
	return order[level] >= order[min]
}
