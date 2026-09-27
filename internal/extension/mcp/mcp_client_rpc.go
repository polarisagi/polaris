package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── 发送 / 等待 ──────────────────────────────────────────────────────────────

// call 发送 JSON-RPC 请求并等待响应；401 挑战且已配置 TokenSource 时失效当前令牌重试一次
// （basic_authorization.md：不构成循环——本函数至多再调用一次 callOnce）。
func (c *MCPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	result, err := c.callOnce(ctx, method, params)
	if err == nil || !c.retryAfterAuthChallenge(err) {
		return result, err
	}
	return c.callOnce(ctx, method, params)
}

// retryAfterAuthChallenge 401 挑战且客户端已配置 TokenSource 时使当前令牌失效，返回是否应重试。
// 403（insufficient_scope）不重试——这需要用户交互完成 step-up 授权，不是简单的令牌刷新。
func (c *MCPClient) retryAfterAuthChallenge(err error) bool {
	var ace *AuthChallengeError
	if !errors.As(err, &ace) || ace.Status != http.StatusUnauthorized {
		return false
	}
	ts := c.tokenSource.Load()
	if ts == nil {
		return false
	}
	(*ts).Invalidate()
	return true
}

// callOnce 发送 JSON-RPC 请求并等待响应（不含重试逻辑，见 call）。
func (c *MCPClient) callOnce(ctx context.Context, method string, params any) (json.RawMessage, error) {
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

func (c *MCPClient) setConfiguredHeaders(ctx context.Context, req *http.Request) {
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	c.applyTokenSourceHeader(ctx, req)
}
