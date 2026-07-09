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

	"github.com/polarisagi/polaris/pkg/apperr"
)

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
