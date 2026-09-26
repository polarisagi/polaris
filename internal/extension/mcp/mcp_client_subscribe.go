package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// subscriptions/listen（spec_subscriptions.md）：新纪元用长连接请求替代旧纪元
// 'notifications/tools/list_changed' 自发通知（旧纪元没有订阅机制，见
// mcp_manager_notify.go attachNotificationSink 的纪元分支）。本文件只实现工具列表
// 变更这一种订阅类型（filter 里的其余字段留给未来的 resources/prompts 订阅复用）。
//
// 两种传输走完全不同的通知到达路径（均由 spec_subscriptions.md 规定）：
//   - Streamable HTTP：subscriptions/listen 的 POST 响应本身就是这次订阅专属的长连接
//     SSE，ack/list_changed/优雅关闭响应都在这一条流里，本文件自己逐事件扫描，命中
//     list_changed 就直接调用调用方传入的 refresh 回调（见 subscribeHTTPOnce）。
//   - stdio：所有消息共用一条 stdout，list_changed 通知与其它服务端消息一样经既有
//     dispatch()/notificationHandler 管道到达（见 mcp_manager_notify.go
//     attachNotificationSink 的 shouldConsumeToolsListChanged 匹配），本文件的
//     subscribeStdioOnce 只负责发请求、登记 pending 以识别优雅关闭。
const (
	methodSubscriptionsListen = "subscriptions/listen"

	// notificationToolsListChanged / notificationSubscriptionsAcknowledged /
	// notificationCancelled 均以完整方法名书写、不拆开引用其子串——子串恰好会以仓库顶层
	// 目录名开头，docs-refs 门控会把它当路径校验（见 comment_refs.go）。
	notificationToolsListChanged          = "notifications/tools/list_changed"
	notificationSubscriptionsAcknowledged = "notifications/subscriptions/acknowledged"
	notificationCancelled                 = "notifications/cancelled"

	// metaSubscriptionID _meta 中标识通知/响应归属哪次 subscriptions/listen 请求的字段名。
	metaSubscriptionID = "io.modelcontextprotocol/subscriptionId"

	// subscriptionBackoffDefaultInitial / subscriptionBackoffDefaultMax Streamable HTTP
	// 订阅异常断线重连的默认退避区间（1s 起，上限 60s）。MCPClient 实例持有可写字段
	// （见 mcp_client.go），此处仅为构造时的默认值；测试可直接改写实例字段缩短等待。
	subscriptionBackoffDefaultInitial = 1 * time.Second
	subscriptionBackoffDefaultMax     = 60 * time.Second

	// subscriptionCancelNotifyTimeout Close() 时尽力发送 notifications/cancelled 的超时。
	subscriptionCancelNotifyTimeout = 2 * time.Second

	// subscriptionSSEAccept 订阅请求的 Accept 头：只接受事件流（普通 JSON 响应意味着
	// 服务器拒绝了这次订阅，见 rejectSubscription）。
	subscriptionSSEAccept = "text/event-stream"
)

// subscriptionEnd 描述一次 subscriptions/listen 尝试的结束方式，决定调用方是否需要
// 指数退避重连。
type subscriptionEnd int

const (
	subscriptionEndAbnormal     subscriptionEnd = iota // 传输异常断开：Streamable HTTP 退避重连；stdio 不重连（既有进程生命周期负责）
	subscriptionEndGraceful                            // 服务器优雅关闭（收到本请求 id 的 JSON-RPC 响应）：不重连
	subscriptionEndUnsupported                         // ack 未包含 toolsListChanged，或服务器直接拒绝了该方法：不重连
	subscriptionEndClientClosed                        // 客户端主动关闭：不重连
)

func (e subscriptionEnd) String() string {
	switch e {
	case subscriptionEndGraceful:
		return "graceful"
	case subscriptionEndUnsupported:
		return "unsupported"
	case subscriptionEndClientClosed:
		return "client_closed"
	default:
		return "abnormal"
	}
}

// subscriptionNotificationFilter subscriptions/listen 请求参数与 ack 响应共用的过滤器
// 结构（spec_subscriptions.md §Notification Filter）。本任务只用 ToolsListChanged。
type subscriptionNotificationFilter struct {
	ToolsListChanged bool `json:"toolsListChanged,omitempty"`
}

// StartToolsListChangedSubscription 异步发起并维持一个工具列表变更订阅（不阻塞调用方）。
// 只应在服务器声明了 ServerMeta.ToolsListChanged 时调用（由 MCPManager.Add 判断）。
// refresh 在收到归属本订阅的 notificationToolsListChanged 时被调用（Streamable HTTP
// 直接调用；stdio 由 MCPManager 经既有通知管道调用，形参在该分支不会被触发，见上方
// 文件级注释）。
func (c *MCPClient) StartToolsListChangedSubscription(refresh func()) {
	concurrent.SafeGo(context.Background(), "mcp_client.subscribe_tools_list_changed", func(context.Context) {
		c.runToolsListChangedSubscription(refresh)
	})
}

// runToolsListChangedSubscription 持续维持订阅：一次尝试结束后按结束方式决定是否退避重连。
// backoff 延迟到第一次真正需要等待时才从 c.subscriptionBackoffInitial 取值（而不是在
// 循环开始前一次性捕获）——同包测试靠"等第一次尝试到达后再改写这两个未导出字段"来
// 缩短退避区间，提前捕获会让改写发生在读取之后而不生效。
func (c *MCPClient) runToolsListChangedSubscription(refresh func()) {
	var backoff time.Duration
	for {
		end, err := c.subscribeToolsListChangedOnce(refresh)
		if err != nil {
			slog.Warn("mcp: subscriptions/listen attempt ended with error", "server", c.cfg.ServerName, "reason", end.String(), "err", err)
		} else {
			slog.Info("mcp: subscriptions/listen attempt ended", "server", c.cfg.ServerName, "reason", end.String())
		}
		if end != subscriptionEndAbnormal || c.cfg.Transport != MCPStreamableHTTP {
			// stdio 异常断开不重连：交给既有进程生命周期处理（进程重启会重新走 Add()）。
			return
		}
		if backoff <= 0 {
			backoff = c.subscriptionBackoffInitial
		}
		slog.Warn("mcp: subscription disconnected abnormally, reconnecting", "server", c.cfg.ServerName, "backoff", backoff)
		if !c.sleepOrClosed(backoff) {
			return // 客户端已关闭
		}
		if next := backoff * 2; next < c.subscriptionBackoffMax {
			backoff = next
		} else {
			backoff = c.subscriptionBackoffMax
		}
	}
}

// sleepOrClosed 等待 d 或客户端关闭，先到者胜；用可停止 timer 而非 time.After，
// 使客户端关闭时能立即结束等待而不是白白占用到 d 走完。
func (c *MCPClient) sleepOrClosed(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-c.done:
		return false
	}
}

// subscribeToolsListChangedOnce 发起一次 subscriptions/listen 并阻塞直至该次尝试结束。
// 只支持 Streamable HTTP 与 stdio（SSE 是已弃用传输，不实现该扩展）。
func (c *MCPClient) subscribeToolsListChangedOnce(refresh func()) (subscriptionEnd, error) {
	switch c.cfg.Transport {
	case MCPStreamableHTTP:
		return c.subscribeHTTPOnce(refresh)
	case MCPStdio:
		return c.subscribeStdioOnce()
	default:
		return subscriptionEndUnsupported, apperr.New(apperr.CodeUnimplemented,
			"mcp: subscriptions/listen unsupported for transport "+string(c.cfg.Transport))
	}
}

// buildSubscriptionRequest 构造 subscriptions/listen 请求体与其 JSON-RPC id。
func (c *MCPClient) buildSubscriptionRequest() (int64, mcpRPCRequest) {
	reqID := c.nextID.Add(1)
	params := c.withMeta(map[string]any{
		"notifications": subscriptionNotificationFilter{ToolsListChanged: true},
	}, modernProtocolVersion)
	return reqID, mcpRPCRequest{JSONRPC: "2.0", ID: &reqID, Method: methodSubscriptionsListen, Params: params}
}

// ─── Streamable HTTP ──────────────────────────────────────────────────────────

// subscribeHTTPOnce 该请求的 POST 响应是长连接 SSE：读取路径不受 c.cfg.Timeout 限制，
// 只受本次尝试自己的 ctx 控制（客户端 Close() 会取消它，见下方 watch goroutine）。
func (c *MCPClient) subscribeHTTPOnce(refresh func()) (subscriptionEnd, error) {
	reqID, rpc := c.buildSubscriptionRequest()
	body, err := json.Marshal(rpc)
	if err != nil {
		return subscriptionEndAbnormal, apperr.Wrap(apperr.CodeInternal, "mcp: marshal subscriptions/listen", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	concurrent.SafeGo(ctx, "mcp_client.subscription_done_watch", func(ctx context.Context) {
		select {
		case <-c.done:
			cancel()
		case <-ctx.Done():
		}
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return subscriptionEndAbnormal, apperr.Wrap(apperr.CodeInternal, "mcp: build subscriptions/listen request", err)
	}
	c.setRequestHeaders(req, rpc)
	req.Header.Set("Accept", subscriptionSSEAccept)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return subscriptionEndClientClosed, nil
		}
		return subscriptionEndAbnormal, apperr.Wrap(apperr.CodeInternal, "mcp: subscriptions/listen request", err)
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return rejectSubscription(resp)
	}
	slog.Info("mcp: subscriptions/listen stream opened", "server", c.cfg.ServerName, "id", reqID)
	return c.readSubscriptionStream(resp.Body, reqID, refresh)
}

// rejectSubscription 服务器未打开事件流：视为不支持该方法，记录原因后结束，不重连。
func rejectSubscription(resp *http.Response) (subscriptionEnd, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return subscriptionEndUnsupported, apperr.Wrap(apperr.CodeInternal, "mcp: read subscriptions/listen rejection", err)
	}
	if resp.StatusCode >= 300 {
		return subscriptionEndUnsupported, apperr.New(apperr.CodeUnimplemented,
			fmt.Sprintf("mcp: subscriptions/listen status %d: %s", resp.StatusCode, b))
	}
	var r mcpRPCResponse
	if json.Unmarshal(b, &r) == nil && r.Error != nil {
		return subscriptionEndUnsupported, apperr.Wrap(apperr.CodeUnimplemented, "mcp: subscriptions/listen rejected", r.Error)
	}
	return subscriptionEndUnsupported, apperr.New(apperr.CodeUnimplemented, "mcp: subscriptions/listen: server did not open an event stream")
}

// readSubscriptionStream 逐事件扫描订阅流，直到收到本请求 id 的 JSON-RPC 响应
// （优雅关闭）或流异常结束。复用 mcp_client_http.go 的 SSE 扫描阀值（同一门控点 L-10）。
func (c *MCPClient) readSubscriptionStream(r io.Reader, reqID int64, refresh func()) (subscriptionEnd, error) {
	maxScan := config.CurrentThresholds().M7Tool.MCPSSEMaxScanBytes
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), maxScan)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				dataLines = append(dataLines, v)
			}
			continue
		}
		if len(dataLines) == 0 {
			continue
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if end, done, err := c.handleSubscriptionEvent([]byte(data), reqID, refresh); done {
			return end, err
		}
	}
	if err := scanner.Err(); err != nil {
		return subscriptionEndAbnormal, apperr.Wrap(apperr.CodeInternal, "mcp: subscription stream scan error", err)
	}
	return subscriptionEndAbnormal, apperr.New(apperr.CodeInternal, "mcp: subscription stream closed without a graceful response")
}

// handleSubscriptionEvent 处理订阅流上的一条事件；done=true 时调用方应结束本次尝试。
func (c *MCPClient) handleSubscriptionEvent(data []byte, reqID int64, refresh func()) (subscriptionEnd, bool, error) {
	var evt mcpRPCResponse
	if err := json.Unmarshal(data, &evt); err != nil {
		slog.Debug("mcp: subscription stream: unparsable event, skipping", "server", c.cfg.ServerName, "err", err)
		return 0, false, nil
	}
	if evt.ID != nil && *evt.ID == reqID {
		if evt.Error != nil {
			slog.Warn("mcp: subscriptions/listen closed with error", "server", c.cfg.ServerName, "err", evt.Error)
		} else {
			slog.Info("mcp: subscriptions/listen closed gracefully by server", "server", c.cfg.ServerName, "id", reqID)
		}
		return subscriptionEndGraceful, true, nil
	}
	switch evt.Method {
	case notificationSubscriptionsAcknowledged:
		return c.handleSubscriptionAck(evt.Params, reqID)
	case notificationToolsListChanged:
		if subscriptionIDMatches(evt.Params, reqID) {
			// 独立 goroutine 触发，避免 refreshTools（内部会同步做 ListTools 网络调用）
			// 阻塞本订阅流的读循环——旧纪元/stdio 侧的同名约束见 mcp_manager_notify.go。
			concurrent.SafeGo(context.Background(), "mcp_client.subscription_refresh", func(context.Context) { refresh() })
		}
	default:
		slog.Debug("mcp: subscription stream: unexpected notification", "server", c.cfg.ServerName, "method", evt.Method)
	}
	return 0, false, nil
}

// handleSubscriptionAck 校验 ack 是否确认了本次请求的 toolsListChanged 过滤器
// （spec_subscriptions.md §Acknowledgment：服务器未支持的类型会从 ack 里省略）。
func (c *MCPClient) handleSubscriptionAck(params json.RawMessage, reqID int64) (subscriptionEnd, bool, error) {
	var ack struct {
		Notifications subscriptionNotificationFilter `json:"notifications"`
	}
	if err := json.Unmarshal(params, &ack); err != nil {
		return subscriptionEndAbnormal, true, apperr.Wrap(apperr.CodeInternal, "mcp: parse subscriptions/acknowledged", err)
	}
	if !ack.Notifications.ToolsListChanged {
		slog.Warn("mcp: server ack did not include toolsListChanged, ending subscription", "server", c.cfg.ServerName, "id", reqID)
		return subscriptionEndUnsupported, true, apperr.New(apperr.CodeUnimplemented, "mcp: subscriptions/listen ack omits toolsListChanged")
	}
	slog.Info("mcp: subscriptions/listen acknowledged", "server", c.cfg.ServerName, "id", reqID)
	return 0, false, nil
}

// ─── stdio ────────────────────────────────────────────────────────────────────

// subscribeStdioOnce 在 stdio 上把 subscriptions/listen 当作"发出即不等待"的普通请求：
// 响应只在服务器优雅关闭订阅时才会到来（可能是几小时后），提前登记到 pending 是为了让
// dispatch() 能把那次响应投递过来，而不是被当成服务端主动请求处理。ack 与
// notificationToolsListChanged 经既有 dispatch()/notificationHandler 管道到达（见
// mcp_manager_notify.go attachNotificationSink 的 shouldConsumeToolsListChanged 匹配），
// 本函数只负责等待"优雅关闭"响应或客户端关闭——不在这里触发 refresh。
func (c *MCPClient) subscribeStdioOnce() (subscriptionEnd, error) {
	reqID, rpc := c.buildSubscriptionRequest()
	ch := make(chan *mcpRPCResponse, 1)
	c.mu.Lock()
	c.pending[reqID] = ch
	c.mu.Unlock()
	c.activeSubscriptionID.Store(reqID)
	defer c.activeSubscriptionID.Store(0)
	defer func() {
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
	}()

	if err := c.send(context.Background(), rpc); err != nil {
		return subscriptionEndAbnormal, apperr.Wrap(apperr.CodeInternal, "mcp: send subscriptions/listen", err)
	}
	slog.Info("mcp: subscriptions/listen sent", "server", c.cfg.ServerName, "id", reqID)

	select {
	case resp := <-ch:
		if resp.Error != nil {
			slog.Warn("mcp: subscriptions/listen closed with error", "server", c.cfg.ServerName, "err", resp.Error)
		} else {
			slog.Info("mcp: subscriptions/listen closed gracefully by server", "server", c.cfg.ServerName, "id", reqID)
		}
		return subscriptionEndGraceful, nil
	case <-c.done:
		return subscriptionEndClientClosed, nil
	}
}

// shouldConsumeToolsListChanged 判断一条经 dispatch() 到达的工具列表变更通知是否应由
// MCPManager 消费触发 refreshTools：旧纪元没有订阅机制，收到即处理；新纪元必须核对
// _meta.subscriptionId 与当前活跃订阅一致——stdio 传输下，订阅通知与其它服务端消息
// 共用同一 dispatch 管道，需要这一步过滤跨订阅串扰。新纪元 Streamable HTTP 传输的
// 订阅走独立读取路径（见 subscribeHTTPOnce），不经过 dispatch，也就不会调用本函数。
func (c *MCPClient) shouldConsumeToolsListChanged(params json.RawMessage) bool {
	if c.protocolEra() != eraModern {
		return true
	}
	id := c.activeSubscriptionID.Load()
	if id == 0 {
		return false
	}
	return subscriptionIDMatches(params, id)
}

// subscriptionIDMatches 解析 params._meta["io.modelcontextprotocol/subscriptionId"] 并
// 与 reqID 比较。
func subscriptionIDMatches(params json.RawMessage, reqID int64) bool {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return false
	}
	raw, ok := p.Meta[metaSubscriptionID]
	if !ok {
		return false
	}
	var id int64
	return json.Unmarshal(raw, &id) == nil && id == reqID
}
