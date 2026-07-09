package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

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
