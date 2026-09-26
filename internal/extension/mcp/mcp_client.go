package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	sandboxpkg "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
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

// mcpRPCError 线上错误对象（与 RPCError 同形）。
type mcpRPCError = RPCError

// MCPTool protocol.MCPTool 本地别名，使包内调用无需显式引用 protocol 包。
type MCPTool = protocol.MCPTool

// MCPClientConfig protocol.MCPClientConfig 本地别名，使包内调用无需显式引用 protocol 包。
type MCPClientConfig = protocol.MCPClientConfig

// MCPClient 实现 MCP JSON-RPC 2.0 协议客户端（stdio + SSE + Streamable HTTP）。
type MCPClient struct {
	cfg        MCPClientConfig
	httpClient network.SafeHTTPClient

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
	// hasSampling / hasElicitation 客户端实际能处理的服务端反向请求类型（MRTR：
	// clientCapabilities 只能声明真正能处理的能力，见 SetInputHandler）。
	hasSampling    bool
	hasElicitation bool

	// serverMeta initialize 结果中的 instructions 与 experimental 能力（channel 声明等）。
	serverMeta atomic.Pointer[ServerMeta]
	// notificationHandler 服务端通知（无 id 的请求）回调；nil 时只记日志。
	notificationHandler atomic.Pointer[NotificationHandler]
	// era 协议纪元（protocolEra）；legacySession 旧纪元 Streamable HTTP 的 Mcp-Session-Id。
	era           atomic.Int32
	legacySession atomic.Pointer[string]
	// toolHeaders 工具名 → x-mcp-header 标注（Streamable HTTP 新纪元 methodToolsCall 镜像到 Mcp-Param-*）。
	toolHeaders sync.Map
}

// ServerMeta MCP 服务器在 initialize 中声明的元数据。
type ServerMeta struct {
	Instructions string
	Experimental map[string]json.RawMessage
	Extensions   map[string]json.RawMessage // 新纪元 capabilities.extensions
}

// DeclaresExperimental 能力值为对象即声明；缺省或 false 为未声明（Claude channel 规则）。
func (m ServerMeta) DeclaresExperimental(key string) bool {
	v, ok := m.Experimental[key]
	return ok && len(v) > 0 && v[0] == '{'
}

// NotificationHandler 服务端通知回调（method、params）。
type NotificationHandler func(method string, params json.RawMessage)

// SetNotificationHandler 注册服务端通知回调（须在 Initialize 前设置，避免丢失早期通知）。
func (c *MCPClient) SetNotificationHandler(h NotificationHandler) { c.notificationHandler.Store(&h) }

// ServerMeta 返回 initialize 结果中的服务器元数据（未初始化时为零值）。
func (c *MCPClient) ServerMeta() ServerMeta {
	if m := c.serverMeta.Load(); m != nil {
		return *m
	}
	return ServerMeta{}
}

// Notify 向服务端发送通知（如 Claude channel 的 permission_request）。
func (c *MCPClient) Notify(ctx context.Context, method string, params any) error {
	return c.notify(ctx, method, params)
}

// SetServerRequestHandler 注册服务端主动请求处理器，不改变已声明的能力位。
// 兼容旧调用点（测试、legacy 场景）；生产装配统一走 SetInputHandler，使
// clientCapabilities 如实反映 handler 实际能处理的方法。
func (c *MCPClient) SetServerRequestHandler(h ServerRequestHandler) {
	c.mu.Lock()
	c.serverReqHandler = h
	c.mu.Unlock()
}

// SetInputHandler 注册统一的服务端反向请求处理器（sampling/elicitation/roots 合一），
// 并显式声明客户端实际支持的能力——MRTR 要求 clientCapabilities 只声明真正能处理的
// 输入请求类型，服务器不得请求未声明的能力（mrtr §Server Requirements 7）。
func (c *MCPClient) SetInputHandler(h ServerRequestHandler, sampling, elicitation bool) {
	c.mu.Lock()
	c.serverReqHandler = h
	c.hasSampling = sampling
	c.hasElicitation = elicitation
	c.mu.Unlock()
}

// declaresCapability 判断客户端是否声明了给定服务端反向请求方法的处理能力。
// roots/list 始终应答（Roots 已弃用、返回空列表无风险），不占用能力声明位。
func (c *MCPClient) declaresCapability(method string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case "sampling/createMessage":
		return c.hasSampling
	case "elicitation/create":
		return c.hasElicitation
	case "roots/list":
		return true
	default:
		return false
	}
}

// NewMCPClient 构造 MCP 客户端。
//
// httpClient 必须是经 SafeDialer 包装的 network.SafeHTTPClient（XR-06：出站网络
// 禁裸 http.Client，全部走 M11 SafeDialer）。此前该形参是裸 *http.Client——生产
// 装配传的确实是 sb.SafeHTTP（SafeDialer 包装后的实例），但类型上无任何约束，
// 任何人塞一个 http.DefaultClient 都能编过，MCP 出站将整体绕过五阶段 SSRF 防护。
// 改用带 isSafe 标记的 SafeHTTPClient + fail-fast 校验，与 marketplace 同一形态。
func NewMCPClient(cfg MCPClientConfig, httpClient network.SafeHTTPClient) *MCPClient {
	if !httpClient.IsSafe() {
		panic("mcp_client: httpClient must be a valid network.SafeHTTPClient (XR-06)")
	}
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
