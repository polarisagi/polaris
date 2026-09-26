package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── 发送 / 等待 ──────────────────────────────────────────────────────────────

// call 发送 JSON-RPC 请求并等待响应。
func (c *MCPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	if c.protocolEra() == eraModern {
		params = c.withMeta(params, modernProtocolVersion)
	}
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
			return nil, apperr.Wrap(apperr.CodeInternal, "MCPClient.call "+method, resp.Error)
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
		return nil, apperr.Wrap(apperr.CodeInternal, "MCPClient.call: context done", ctx.Err())
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
		return c.httpPostOnly(ctx, c.postURL, b, req)
	case MCPStreamableHTTP:
		resp, err := c.httpPostReceive(ctx, c.cfg.URL, b, req)
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

func (c *MCPClient) setConfiguredHeaders(req *http.Request) {
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
}
