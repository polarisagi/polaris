package edge

import "context"

// CLI — 流式 REPL 入口。
// 架构文档: docs/arch/13-Interface-Scheduler-深度选型.md §1.1

// AgentREPL 交互式 Agent REPL。
type AgentREPL struct {
	history []REPLEntry
	session *Session
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
	ID        string
	CreatedAt int64
	UpdatedAt int64
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

// Run REPL 主循环。
// 1. 欢迎提示
// 2. 逐行读 stdin
// 3. /<cmd>→handleCommand; 否则→订阅 agent.StreamInfer
// 4. EventToken→stdout; EventToolCall→"calling {name}"; EventThinking→思考; EventComplete→结束
func (repl *AgentREPL) Run(ctx context.Context) error {
	return nil
}

// RateLimiterMiddleware 双层隔离限流 (GCRA Token Bucket).
// 进程指纹: 本地→PID+启动时间 hash; 远程→Ed25519 AgentIdentity 公钥 hash.
// 熔断: 连续 3 个 1s 窗口>100%配额→隔离 30s (429+Retry-After:30).
type RateLimiterMiddleware struct {
	limits   map[string]*RateLimit // fingerprint+client_type → limit
	breakers map[string]*RateBreaker
}

// GetBreaker 获取或创建对应 key 的熔断器。
func (rlm *RateLimiterMiddleware) GetBreaker(key string) *RateBreaker {
	if b, ok := rlm.breakers[key]; ok {
		return b
	}
	if rlm.breakers == nil {
		rlm.breakers = make(map[string]*RateBreaker)
	}
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

// Admit 准入检查。
func (rlm *RateLimiterMiddleware) Admit(fingerprint, clientType string) bool {
	key := fingerprint + ":" + clientType
	_, ok := rlm.limits[key]
	return ok
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
