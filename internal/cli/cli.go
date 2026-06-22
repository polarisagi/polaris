package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// CLI — 流式 REPL 入口。
// 架构文档: docs/arch/13-Interface-Scheduler-深度选型.md §1.1

// AgentREPL 交互式 Agent REPL。
type AgentREPL struct {
	history []REPLEntry
	session *Session
	InferFn func(ctx context.Context, input string) (<-chan types.StreamEvent, error)
}

// AddHistory 增加 REPL 历史。
func (repl *AgentREPL) AddHistory(entry REPLEntry) {
	repl.history = append(repl.history, entry)
}

// SetSession 设置会话。
func (repl *AgentREPL) SetSession(s *Session) {
	repl.session = s
}

// REPLEntry REPL 历史条目。
type REPLEntry struct {
	Input     string
	Output    string
	ToolCalls []string
}

// Session 会话。
type Session struct {
	ID             string
	CreatedAt      int64
	UpdatedAt      int64
	ThrashingIndex float64
}

// SubCommands CLI 子命令。
const (
	CmdQuery    = "query"
	CmdChat     = "chat"
	CmdServe    = "serve"
	CmdConfig   = "config"
	CmdCron     = "cron"
	CmdSessions = "sessions"
	CmdStatus   = "status"
	CmdDoctor   = "doctor"
)

// Run REPL 主循环（M13 §1.1）。
// 逐行读 stdin；"/" 前缀→内置命令；否则→输出到 stdout（StreamInfer 订阅由调用方注入）。
func (repl *AgentREPL) Run(ctx context.Context) error {
	fmt.Fprintln(os.Stdout, "Polaris Agent REPL — /help 查看命令，Ctrl+D 或 /quit 退出")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		fmt.Fprint(os.Stdout, "> ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				fmt.Fprintf(os.Stdout, "[Error] %v\n", err)
			}
			// EOF (Ctrl+D) or Error
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			if done := repl.handleCommand(ctx, line); done {
				return nil
			}
			continue
		}

		// 普通输入 → 追加历史，输出 echo（StreamInfer 由调用方扩展注入）
		if repl.InferFn != nil {
			ch, err := repl.InferFn(ctx, line)
			if err != nil {
				fmt.Fprintf(os.Stdout, "[Error] %v\n", err)
				repl.AddHistory(REPLEntry{Input: line, Output: fmt.Sprintf("error: %v", err)})
				continue
			}
			var sb strings.Builder
			for ev := range ch {
				if ev.Type == types.StreamTextDelta {
					fmt.Fprint(os.Stdout, ev.Content)
					sb.WriteString(ev.Content)
				}
				if ev.Type == types.StreamError || ev.Type == types.StreamCancelled {
					fmt.Fprintf(os.Stdout, "\n[Stream interrupted: %v]\n", ev.Content)
					break
				}
			}
			fmt.Fprintln(os.Stdout)
			repl.AddHistory(REPLEntry{Input: line, Output: sb.String()})
		} else {
			fmt.Fprintln(os.Stdout, "[Agent] 推理管道未注入，请检查 AgentREPL.InferFn 配置")
			repl.AddHistory(REPLEntry{Input: line, Output: "[InferFn not injected]"})
		}
	}
}

// handleCommand 处理 "/" 前缀内置命令。返回 true 表示应退出 REPL。
func (repl *AgentREPL) handleCommand(_ context.Context, cmd string) (exit bool) {
	switch strings.ToLower(strings.TrimPrefix(cmd, "/")) {
	case "quit", "exit", "q":
		fmt.Fprintln(os.Stdout, "再见。")
		return true
	case "help", "h":
		fmt.Fprintln(os.Stdout, "命令: /help /quit /sessions /status /memory /skills")
	case "sessions":
		if repl.session != nil {
			fmt.Fprintf(os.Stdout, "当前会话: %s\n", repl.session.ID)
		} else {
			fmt.Fprintln(os.Stdout, "无活跃会话")
		}
	case "status":
		fmt.Fprintf(os.Stdout, "历史条目: %d\n", len(repl.history))
	default:
		fmt.Fprintf(os.Stdout, "未知命令: %s\n", cmd)
	}
	return false
}

// RateLimiterMiddleware 双层隔离限流 (GCRA Token Bucket).
// 进程指纹: 本地→PID+启动时间 hash; 远程→Ed25519 AgentIdentity 公钥 hash.
// 熔断: 连续 3 个 1s 窗口>100%配额→隔离 30s (429+Retry-After:30).
type RateLimiterMiddleware struct {
	mu       sync.Mutex
	limits   map[string]*RateLimit // fingerprint+client_type → limit
	breakers map[string]*RateBreaker
	gcra     map[string]*gcraState // GCRA 每 key 状态
	lastSeen map[string]time.Time  // 记录每个 key 最后活跃时间
}

// NewRateLimiterMiddleware creates a new rate limiter middleware and starts its cleanup loop.
func NewRateLimiterMiddleware() *RateLimiterMiddleware {
	r := &RateLimiterMiddleware{
		limits:   make(map[string]*RateLimit),
		breakers: make(map[string]*RateBreaker),
		gcra:     make(map[string]*gcraState),
		lastSeen: make(map[string]time.Time),
	}
	go r.cleanupLoop()
	return r
}

// cleanupLoop 每 15 分钟清理超过 30 分钟未活跃的 key。
func (rlm *RateLimiterMiddleware) cleanupLoop() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rlm.mu.Lock()
		cutoff := time.Now().Add(-30 * time.Minute)
		for k, t := range rlm.lastSeen {
			if t.Before(cutoff) {
				delete(rlm.limits, k)
				delete(rlm.breakers, k)
				delete(rlm.gcra, k)
				delete(rlm.lastSeen, k)
			}
		}
		rlm.mu.Unlock()
	}
}

// gcraState GCRA Token Bucket 状态。
type gcraState struct {
	tat int64 // Theoretical Arrival Time（纳秒 Unix 时间戳）
}

// GetBreaker 获取或创建对应 key 的熔断器。
func (rlm *RateLimiterMiddleware) GetBreaker(key string) *RateBreaker {
	if b, ok := rlm.breakers[key]; ok {
		return b
	}
	if rlm.breakers == nil {
		rlm.breakers = make(map[string]*RateBreaker)
	}
	if rlm.lastSeen == nil {
		rlm.lastSeen = make(map[string]time.Time)
	}
	rlm.lastSeen[key] = time.Now()
	b := &RateBreaker{}
	rlm.breakers[key] = b
	return b
}

// RateLimit 限流配置。
type RateLimit struct {
	QuotaPerSec int // CLI 50/s, WebUI 30/s, A2A 30/s, WS 5/s, gRPC 50/s/method, /_admin/ 10/s
	BurstAllow  int
}

// RateBreaker 熔断器。
type RateBreaker struct {
	consecutiveOver int
	isolatedUntil   int64
}

// Record 记录是否超限。
func (rb *RateBreaker) Record(isOver bool) {
	if isOver {
		rb.consecutiveOver++
	} else {
		rb.consecutiveOver = 0
	}
}

// IsIsolated 检查当前时间是否被隔离。
func (rb *RateBreaker) IsIsolated(now int64) bool {
	return now < rb.isolatedUntil
}

// SetIsolatedUntil 设置隔离截止时间。
func (rb *RateBreaker) SetIsolatedUntil(t int64) {
	rb.isolatedUntil = t
}

// Admit 准入检查（GCRA Token Bucket）。
// 返回 false 表示超限，调用方应返回 429。
func (rlm *RateLimiterMiddleware) Admit(fingerprint, clientType string) bool {
	key := fingerprint + ":" + clientType

	rlm.mu.Lock()
	defer rlm.mu.Unlock()

	if rlm.lastSeen == nil {
		rlm.lastSeen = make(map[string]time.Time)
	}
	rlm.lastSeen[key] = time.Now()

	limit, ok := rlm.limits[key]
	if !ok {
		// 未配置 limit 的 key 默认放行
		return true
	}

	if rlm.gcra == nil {
		rlm.gcra = make(map[string]*gcraState)
	}

	now := time.Now().UnixNano()
	emissionInterval := int64(time.Second) / int64(limit.QuotaPerSec)
	burst := int64(limit.BurstAllow)
	delayTolerance := emissionInterval * burst

	state, exists := rlm.gcra[key]
	if !exists {
		state = &gcraState{tat: now}
		rlm.gcra[key] = state
	}

	newTAT := state.tat + emissionInterval
	if now > newTAT {
		newTAT = now + emissionInterval
	}

	// 超限判断：新 TAT 超出容忍窗口
	if newTAT-now > delayTolerance+emissionInterval {
		// 熔断器记录
		if rlm.breakers == nil {
			rlm.breakers = make(map[string]*RateBreaker)
		}
		if _, ok := rlm.breakers[key]; !ok {
			rlm.breakers[key] = &RateBreaker{}
		}
		rlm.breakers[key].Record(true)
		if rlm.breakers[key].consecutiveOver >= 3 {
			rlm.breakers[key].SetIsolatedUntil(now + int64(30*time.Second))
		}
		return false
	}

	// 检查是否被隔离
	if b, ok := rlm.breakers[key]; ok && b.IsIsolated(now) {
		return false
	}

	state.tat = newTAT
	if b, ok := rlm.breakers[key]; ok {
		b.Record(false)
	}
	return true
}

// WebSocketHub WebSocket 广播中心。
// cap=256 队列, 分级背压:
//
//	Critical(不可丢弃): tool_call_started, tool_result, error, approval_required, task_completed, task_failed
//	Streaming(可丢弃): token, thinking
type WebSocketHub struct {
	clients    map[string]*WSClient
	broadcast  chan WSEvent
	register   chan *WSClient
	unregister chan *WSClient
}

// NewWebSocketHub 创建 WebSocketHub。
func NewWebSocketHub() *WebSocketHub {
	return &WebSocketHub{
		clients:    make(map[string]*WSClient),
		broadcast:  make(chan WSEvent, 256),
		register:   make(chan *WSClient),
		unregister: make(chan *WSClient),
	}
}

// Run 启动事件分发循环。
func (hub *WebSocketHub) Run(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case client := <-hub.register:
				hub.clients[client.ID] = client
			case client := <-hub.unregister:
				if _, ok := hub.clients[client.ID]; ok {
					delete(hub.clients, client.ID)
					close(client.Send)
				}
			case message := <-hub.broadcast:
				for _, client := range hub.clients {
					select {
					case client.Send <- message:
					default:
						close(client.Send)
						delete(hub.clients, client.ID)
					}
				}
			}
		}
	}()
}

// WSClient WebSocket 客户端。
type WSClient struct {
	ID      string
	Send    chan WSEvent
	Session *Session
}

// WSEvent WebSocket 事件。
type WSEvent struct {
	Type      string
	Data      any
	Timestamp int64
}

// CoalesceEvents 背压合并: 连续 Streaming event → 合并为单条 text。
// Go struct 层面合并, WriteJSON 仅在合并后; 不对已序列化 []byte 拼接。
func (hub *WebSocketHub) CoalesceEvents(events []WSEvent) []WSEvent {
	var result []WSEvent
	var coalesced *WSEvent
	for i := range events {
		if events[i].Type == "token" || events[i].Type == "thinking" {
			if coalesced == nil {
				coalesced = &events[i]
			}
			// 合并 streaming event
		} else {
			result = append(result, events[i])
		}
	}
	if coalesced != nil {
		result = append(result, *coalesced)
	}
	return result
}
