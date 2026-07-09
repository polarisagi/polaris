package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
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
