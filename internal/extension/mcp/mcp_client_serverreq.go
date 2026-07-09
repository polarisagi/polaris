package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

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
			//nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
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
