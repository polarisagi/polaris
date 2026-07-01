package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	sandboxpkg "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"

	"github.com/polarisagi/polaris/pkg/types"
)

// ─── JSON-RPC 2.0 ─────────────────────────────────────────────────────────────

type mcpRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type ServerRequestHandler func(ctx context.Context, method string, id int64, params json.RawMessage) (json.RawMessage, error)

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPTool protocol.MCPTool 本地别名，使包内调用无需显式引用 protocol 包。
type MCPTool = protocol.MCPTool

// MCPClientConfig protocol.MCPClientConfig 本地别名，使包内调用无需显式引用 protocol 包。
type MCPClientConfig = protocol.MCPClientConfig

// MCPClient 实现 MCP JSON-RPC 2.0 协议客户端（stdio + SSE + Streamable HTTP）。
type MCPClient struct {
	cfg        MCPClientConfig
	httpClient *http.Client

	// stdio 专用
	cmd   *exec.Cmd
	stdin io.WriteCloser

	// SSE 专用（从 endpoint 事件获取 POST URL）
	postURL string

	// 请求等待表
	mu      sync.Mutex
	pending map[int64]chan *mcpRPCResponse
	nextID  atomic.Int64

	done chan struct{}
	once sync.Once

	serverReqHandler ServerRequestHandler
}

// SetServerRequestHandler 注册服务端主动请求处理器。
func (c *MCPClient) SetServerRequestHandler(h ServerRequestHandler) {
	c.mu.Lock()
	c.serverReqHandler = h
	c.mu.Unlock()
}

func NewMCPClient(cfg MCPClientConfig, httpClient *http.Client) *MCPClient {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &MCPClient{
		cfg:        cfg,
		httpClient: httpClient,
		pending:    make(map[int64]chan *mcpRPCResponse),
		done:       make(chan struct{}),
	}
}

// Connect 建立传输层连接并启动响应读取循环。
func (c *MCPClient) Connect(ctx context.Context) error {
	switch c.cfg.Transport {
	case MCPStdio:
		return c.connectStdio(ctx)
	case MCPSSE:
		return c.connectSSE(ctx)
	case MCPStreamableHTTP:
		return nil // HTTP 无持久连接，每次请求独立建立
	default:
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: unsupported transport %q", c.cfg.Transport))
	}
}

// buildSandboxedMCPCmd 构建已沙箱封装的 exec.Cmd（统一 Rust 沙箱）。
//
// 策略：
//   - SandboxPolicy=="none" 或 TrustTier>=3 → bare exec（信任来源/显式退出）
//   - 其他 → 调用 RustSandboxWrapArgv 获取平台沙箱 argv
//
// 失败时直接返回 error 拒绝启动（Fail-Closed）。
// 网络策略：TrustTier<=2 默认 deny；RequiresNetwork+NetworkApproved 时 allow。
func buildSandboxedMCPCmd(cfg MCPClientConfig) (*exec.Cmd, error) {
	// 显式退出沙箱 / 可信来源 → bare exec
	if cfg.SandboxPolicy == "none" || cfg.TrustTier >= 3 {
		cmd := exec.Command(cfg.Command, cfg.Args...) //nolint:gosec
		if cfg.WorkDir != "" {
			cmd.Dir = cfg.WorkDir
		}
		cmd.Env = sanitizeParentEnv()
		return cmd, nil
	}

	// 网络策略：低信任默认断网，审批通过后放行
	netPolicy := protocol.NetPolicyDeny
	if cfg.RequiresNetwork && cfg.NetworkApproved {
		netPolicy = protocol.NetPolicyAllow
	}

	var allowedPaths []string
	if cfg.WorkDir != "" {
		allowedPaths = append(allowedPaths, cfg.WorkDir)
	}

	ctx := protocol.SandboxContext{
		CallerType:    protocol.CallerMCP,
		ExecPath:      cfg.Command,
		ExecArgs:      cfg.Args,
		Workdir:       cfg.WorkDir,
		AllowedPaths:  allowedPaths,
		NetworkPolicy: netPolicy,
	}

	result, err := sandboxpkg.RustSandboxWrapArgv(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("mcp: sandbox wrap failed for server %q, refusing to start unsandboxed (fail-closed)", cfg.ServerName), err)
	}

	cmd := exec.Command(result.Executable, result.Argv...) //nolint:gosec
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	if result.EnvInArgv {
		// bwrap：env 已通过 --setenv 嵌入 argv，cmd.Env 设 nil（继承空环境）
		cmd.Env = []string{}
	} else {
		// seatbelt/bare：env 通过 cmd.Env 注入
		cmd.Env = result.Env
	}
	slog.Info("mcp: stdio process wrapped with Rust sandbox",
		"method", result.SandboxMethod, "server", cfg.ServerName,
		"trust_tier", cfg.TrustTier, "net_isolated", result.NetIsolated,
		"requires_network", cfg.RequiresNetwork, "network_approved", cfg.NetworkApproved)
	return cmd, nil
}

// ─── stdio transport ──────────────────────────────────────────────────────────

func (c *MCPClient) connectStdio(ctx context.Context) error {
	if c.cfg.Command == "" {
		return apperr.New(apperr.CodeInternal, "mcp: stdio transport requires command")
	}
	// 使用 exec.Command 而非 exec.CommandContext：子进程生命周期不绑定 60s 握手超时 ctx，
	// 避免 defer cancel() 在 Add() 返回后立即杀死已就绪的 MCP 子进程（context-kill bug）。
	// 子进程由 Close() 显式终止。
	_ = ctx // ctx 仅用于握手阶段读写超时，不控制进程生命周期

	// buildSandboxedMCPCmd 已内部调用 sanitizeParentEnv 并应用 Rust 沙箱封装
	cmd, sandboxErr := buildSandboxedMCPCmd(c.cfg)
	if sandboxErr != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: build sandboxed cmd: %v", sandboxErr), sandboxErr)
	}
	// 叠加服务器级显式配置变量（优先级最高，覆盖 sanitizeParentEnv 基础集）
	for k, v := range c.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: stdin pipe: %v", err), err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: stdout pipe: %v", err), err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: stderr pipe: %v", err), err)
	}
	if err := cmd.Start(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: start process: %v", err), err)
	}
	c.cmd = cmd
	c.stdin = stdin
	concurrent.SafeGo(context.Background(), "mcp_client_readloop", func(_ context.Context) {
		c.readLoop(stdout)
	})
	// stderr 升级到 Warn 级别：子进程崩溃原因（缺失依赖、Python/Node 错误）在生产日志可见
	concurrent.SafeGo(context.Background(), "mcp_client_stderr", func(_ context.Context) {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			slog.Warn("mcp: server stderr", "server", c.cfg.ServerName, "line", sc.Text())
		}
		if err := sc.Err(); err != nil {
			slog.Warn("mcp: server stderr scan error", "server", c.cfg.ServerName, "err", err)
		}
	})
	return nil
}

// readLoop 持续读取 stdout，dispatch JSON-RPC 响应。
func (c *MCPClient) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var resp mcpRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			slog.Debug("mcp: stdio parse error", "err", err)
			continue
		}
		c.dispatch(&resp)
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("mcp: readLoop scan error", "server", c.cfg.ServerName, "err", err)
	}
	c.Close()
}

// ─── SSE transport ────────────────────────────────────────────────────────────

func (c *MCPClient) connectSSE(ctx context.Context) error {
	sseURL := strings.TrimRight(c.cfg.URL, "/") + "/sse"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MCPClient.connectSSE", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: SSE connect", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: SSE status %d", resp.StatusCode))
	}

	endpointCh := make(chan string, 1)
	go c.readSSE(resp.Body, endpointCh)

	select {
	case postURL := <-endpointCh:
		c.postURL = postURL
		return nil
	case <-time.After(10 * time.Second):
		resp.Body.Close()
		return apperr.New(apperr.CodeInternal, "mcp: SSE endpoint event timeout")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *MCPClient) readSSE(body io.ReadCloser, endpointCh chan<- string) {
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var event string
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// 事件边界：SSE 规范要求多行 data 以 \n 拼接
			data := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]
			switch event {
			case "endpoint":
				select {
				case endpointCh <- data:
				default:
				}
			case "message", "":
				var resp mcpRPCResponse
				if err := json.Unmarshal([]byte(data), &resp); err == nil {
					c.dispatch(&resp)
				}
			}
			event = ""
			continue
		}
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			event = v
		} else if v, ok := strings.CutPrefix(line, "data: "); ok {
			dataLines = append(dataLines, v)
		}
		// id: / retry: 字段当前不需要处理，忽略
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("mcp: readSSE scan error", "server", c.cfg.ServerName, "err", err)
	}
	c.Close()
}

// ─── 发送 / 等待 ──────────────────────────────────────────────────────────────

// call 发送 JSON-RPC 请求并等待响应。
func (c *MCPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	req := mcpRPCRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}

	ch := make(chan *mcpRPCResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(ctx, req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, apperr.Wrap(apperr.CodeInternal, "MCPClient.call", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp rpc error %d: %s", resp.Error.Code, resp.Error.Message))
		}
		return resp.Result, nil
	case <-time.After(c.cfg.Timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: request timeout (%s)", c.cfg.Timeout))
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, apperr.New(apperr.CodeInternal, "mcp: connection closed")
	}
}

func (c *MCPClient) notify(ctx context.Context, method string, params any) error {
	req := mcpRPCRequest{JSONRPC: "2.0", Method: method, Params: params}
	return c.send(ctx, req)
}

func (c *MCPClient) send(ctx context.Context, req mcpRPCRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MCPClient.send", err)
	}
	switch c.cfg.Transport {
	case MCPStdio:
		_, err = c.stdin.Write(append(b, '\n'))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "MCPClient.send", err)
		}
		return nil
	case MCPSSE:
		return c.httpPostOnly(ctx, c.postURL, b)
	case MCPStreamableHTTP:
		resp, err := c.httpPostReceive(ctx, c.cfg.URL, b)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "MCPClient.send", err)
		}
		if resp != nil {
			c.dispatch(resp)
		}
		return nil
	}
	return apperr.New(apperr.CodeInternal, "mcp: unknown transport")
}

// setMCPHeaders 在 HTTP 请求上设置 MCP 规范要求的请求头。
// MCP 2025-11-25 §Transports：HTTP 模式下所有请求必须携带 MCP-Protocol-Version。
func (c *MCPClient) setMCPHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
}

func (c *MCPClient) httpPostOnly(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MCPClient.httpPostOnly", err)
	}
	c.setMCPHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MCPClient.httpPostOnly", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: POST status %d: %s", resp.StatusCode, b))
	}
	return nil
}

// httpPostReceive 向 Streamable HTTP endpoint POST，读取 JSON 或 SSE 响应。
// SSE 模式：扫描流中所有事件，返回首个 id 匹配的 RPC 响应（通知事件异步 dispatch）。
func (c *MCPClient) httpPostReceive(ctx context.Context, url string, body []byte) (*mcpRPCResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "MCPClient.httpPostReceive", err)
	}
	c.setMCPHeaders(req)
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "MCPClient.httpPostReceive", err)
	}
	defer resp.Body.Close()

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return c.readSSESingleResponse(resp.Body)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "MCPClient.httpPostReceive", err)
	}
	var r mcpRPCResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: response parse: %v", err), err)
	}
	return &r, nil
}

func (c *MCPClient) readSSESingleResponse(r io.Reader) (*mcpRPCResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(dataLines) == 0 {
				continue
			}
			// 事件边界：合并多行 data（SSE 规范：多行 data 以 \n 连接）
			data := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]
			var resp mcpRPCResponse
			if json.Unmarshal([]byte(data), &resp) != nil {
				continue
			}
			// 有 ID 的是 RPC 响应；无 ID 的是通知，异步 dispatch
			if resp.ID != nil {
				return &resp, nil
			}
			c.dispatch(&resp)
			continue
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			dataLines = append(dataLines, v)
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("mcp: streamable http SSE scan error", "server", c.cfg.ServerName, "err", err)
	}
	return nil, apperr.New(apperr.CodeInternal, "mcp: streamable http: no rpc response in SSE stream")
}

func (c *MCPClient) dispatch(resp *mcpRPCResponse) {
	if resp.ID == nil {
		if resp.Method != "" {
			slog.Debug("mcp: server notification", "server", c.cfg.ServerName, "method", resp.Method)
		}
		return
	}
	if resp.Method != "" {
		c.mu.Lock()
		_, inPending := c.pending[*resp.ID]
		handler := c.serverReqHandler
		c.mu.Unlock()
		if !inPending {
			go c.handleServerRequest(resp.Method, *resp.ID, resp.Params, handler)
			return
		}
	}
	c.mu.Lock()
	ch, ok := c.pending[*resp.ID]
	if ok {
		delete(c.pending, *resp.ID)
	}
	c.mu.Unlock()
	if ok {
		select {
		case ch <- resp:
		default:
		}
	}
}

// handleServerRequest 在独立 goroutine 中处理服务端主动请求，回复 JSON-RPC 响应。
func (c *MCPClient) handleServerRequest(method string, id int64, params json.RawMessage, handler ServerRequestHandler) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result json.RawMessage
	var rpcErr *mcpRPCError

	if handler != nil {
		var err error
		result, err = handler(ctx, method, id, params)
		if err != nil {
			rpcErr = &mcpRPCError{Code: -32603, Message: err.Error()}
		}
	} else {
		// 无 handler：返回 MethodNotFound
		rpcErr = &mcpRPCError{Code: -32601, Message: "method not found: " + method}
	}

	resp := mcpRPCRequest{
		JSONRPC: "2.0",
		ID:      &id,
	}
	_ = resp // suppress unused
	// 通过 postRaw 发送响应（复用现有发送路径）
	payload, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *mcpRPCError    `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
	_ = c.postRaw(ctx, payload)
}

func (c *MCPClient) postRaw(ctx context.Context, b []byte) error {
	switch c.cfg.Transport {
	case MCPStdio:
		_, err := c.stdin.Write(append(b, '\n'))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "MCPClient.postRaw", err)
		}
		return nil
	case MCPSSE:
		return c.httpPostOnly(ctx, c.postURL, b)
	case MCPStreamableHTTP:
		resp, err := c.httpPostReceive(ctx, c.cfg.URL, b)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "MCPClient.postRaw", err)
		}
		if resp != nil {
			c.dispatch(resp)
		}
		return nil
	}
	return apperr.New(apperr.CodeInternal, "mcp: unknown transport")
}

// ─── MCP 协议方法 ─────────────────────────────────────────────────────────────

// mcpProtocolVersion 当前实现支持的 MCP 协议版本（2025-11-25 为当前稳定版本）。
const mcpProtocolVersion = "2025-11-25"

// Initialize 执行 MCP 初始化握手，校验服务器返回的协议版本。
func (c *MCPClient) Initialize(ctx context.Context) error {
	caps := map[string]any{}
	if c.serverReqHandler != nil {
		caps["roots"] = map[string]any{"listChanged": false}
		caps["sampling"] = map[string]any{}
	}
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    caps,
		"clientInfo":      map[string]any{"name": "polaris", "version": "1.0"},
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: initialize", err)
	}
	// 校验服务器返回的协议版本（规范要求：不支持则应断连）
	var initResp struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &initResp) == nil && initResp.ProtocolVersion != "" {
		if initResp.ProtocolVersion != mcpProtocolVersion {
			slog.Warn("mcp: server protocol version mismatch",
				"server", initResp.ProtocolVersion, "client", mcpProtocolVersion)
			// 仅警告不中断：允许向下兼容旧版服务器（2024-11-05）
		}
	}
	return c.notify(ctx, "notifications/initialized", nil)
}

// mcpContentBlock MCP 工具响应的 content block。
// MCP spec 定义两种主要类型：
//   - type="text": {type, text}
//   - type="image": {type, data (base64), mimeType}
//
// 参考：MCP 2025-11-25 §Tools/CallTool Response
type mcpContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`     // image block: base64 编码的图片数据
	MIMEType string `json:"mimeType,omitempty"` // image block: 如 "image/jpeg"
}

// parseMCPContent 解析 MCP content block 列表，分离文本和图片。
// image block 的 base64 data 解码为原始字节构造 types.ImagePart。
// 解码失败的 image block 记录警告日志后跳过，不中断流程。
func parseMCPContent(blocks []mcpContentBlock) (text string, imgs []types.ImagePart) {
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "image":
			if b.Data == "" || b.MIMEType == "" {
				slog.Warn("mcp: image content block missing data or mimeType, skipping")
				continue
			}
			// base64 → 原始字节（ImagePart.Data 约定为 raw bytes，非 base64）
			raw, err := decodeBase64(b.Data)
			if err != nil {
				slog.Warn("mcp: failed to decode image content block", "err", err)
				continue
			}
			imgs = append(imgs, types.ImagePart{
				Type:      "image",
				MediaType: b.MIMEType,
				Data:      raw,
			})
		default:
			// 未知类型（embedded_resource 等）静默跳过，保持向前兼容
			slog.Debug("mcp: unknown content block type, skipping", "type", b.Type)
		}
	}
	return sb.String(), imgs
}

// decodeBase64 尝试标准 base64 解码，失败时回退 URL-safe 变体。
// MCP 服务器实现差异：部分使用标准 +/（StdEncoding），部分使用 URL-safe -_（RawURLEncoding）。
func decodeBase64(s string) ([]byte, error) {
	// 先尝试标准编码（含 padding）
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	// 再尝试 URL-safe 无 padding 编码（RFC 4648 §5）
	return base64.RawURLEncoding.DecodeString(s)
}

// ListTools 查询服务端工具列表。
func (c *MCPClient) ListTools(ctx context.Context) ([]MCPTool, error) {
	result, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: tools/list", err)
	}
	var resp struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/list parse: %v", err), err)
	}
	return resp.Tools, nil
}

// CallTool 调用指定工具并返回文本和图片结果。
func (c *MCPClient) CallTool(ctx context.Context, name string, arguments map[string]any) (string, []types.ImagePart, error) {
	result, err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return "", nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call %q", name), err)
	}
	var resp struct {
		Content []mcpContentBlock `json:"content"`
		IsError bool              `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call parse: %v", err), err)
	}
	text, imgs := parseMCPContent(resp.Content)
	if resp.IsError {
		return "", nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: tool error: %s", text))
	}
	return text, imgs, nil
}

// CallToolTainted 调用工具，对响应 JSON 进行污点保护反序列化，返回内容、图片、最高污点等级。
//
// 依赖 TaintPreservingDecoder 对所有 string 叶子打标（M07 §1 安全要求）。
// trusted 由 MCPClientConfig.Trusted 决定：白名单 → TaintMedium；其余 → TaintHigh。
func (c *MCPClient) CallToolTainted(ctx context.Context, name string, arguments map[string]any) (string, []types.ImagePart, types.TaintLevel, error) {
	result, err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return "", nil, types.TaintHigh, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call %q", name), err)
	}

	// 污点保护反序列化：遍历 JSON 树，对所有 string 叶子打标
	dec := NewTaintPreservingDecoder(c.cfg.ServerName, c.cfg.Trusted)
	node := dec.Decode(result, "")
	maxTaint := node.MaxTaint()
	if maxTaint < dec.Taint() {
		// 若 JSON 全为非 string 节点（无叶子字符串），仍保守取 server 级别
		maxTaint = dec.Taint()
	}

	var resp struct {
		Content []mcpContentBlock `json:"content"`
		IsError bool              `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", nil, maxTaint, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call parse: %v", err), err)
	}
	text, imgs := parseMCPContent(resp.Content)
	if resp.IsError {
		return "", nil, maxTaint, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: tool error: %s", text))
	}
	return text, imgs, maxTaint, nil
}

// Close 关闭连接并释放资源。
func (c *MCPClient) Close() {
	c.once.Do(func() {
		close(c.done)
		if c.stdin != nil {
			c.stdin.Close()
		}
		if c.cmd != nil {
			// 先显式 Kill 再 Wait，防止子进程僵尸（exec.Command 不自动回收）
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			if err := c.cmd.Wait(); err != nil {
				slog.Warn("mcp: server process exited", "server", c.cfg.ServerName,
					"exit_code", c.cmd.ProcessState.ExitCode(), "err", err)
			}
		}
	})
}
