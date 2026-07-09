package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

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
	concurrent.SafeGo(ctx, "mcp_client.read_sse", func(context.Context) {
		c.readSSE(resp.Body, endpointCh)
	})

	select {
	case postURL := <-endpointCh:
		c.postURL = postURL
		return nil
	case <-time.After(10 * time.Second):
		resp.Body.Close()
		return apperr.New(apperr.CodeInternal, "mcp: SSE endpoint event timeout")
	case <-ctx.Done():
		return apperr.Wrap(apperr.CodeInternal, "MCPClient.connectSSE: context done", ctx.Err())
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
