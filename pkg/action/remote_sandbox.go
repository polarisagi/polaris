package action

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// ============================================================================
// Remote Sandbox（L4）
// 参照: hermes-agent Modal/Daytona/Vercel terminal backend
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.4（待补）
//
// 用途：Tier-0 本地内存不足时，将重计算任务 HTTP 委托给远端执行器。
// 远端可以是自托管 VPS、AWS Lambda、Modal、Daytona 等任意 HTTP 端点。
//
// 安全约束：出站 HTTP 需经 SafeDialer（XR-06）。
// ============================================================================

// RemoteExecRequest 发送给远端执行器的请求体。
type RemoteExecRequest struct {
	ToolName    string                   `json:"tool_name"`
	Input       []byte                   `json:"input"`
	Capability  protocol.CapabilityLevel `json:"capability"`
	SideEffects []protocol.SideEffect    `json:"side_effects,omitempty"`
	CPUQuotaMs  int                      `json:"cpu_quota_ms"`
}

// RemoteSandbox 将工具执行委托给远端 HTTP 执行器。
// 实现 SandboxProvider 接口，路由优先级：SandboxRemote > SandboxContainer。
type RemoteSandbox struct {
	endpoint   string
	authToken  string
	httpClient *http.Client
}

// NewRemoteSandbox 创建 Remote Sandbox。
//
//	endpoint:   远端执行器根 URL，如 "https://executor.example.com"
//	authToken:  Bearer 认证令牌（空字符串 = 无认证）
//	timeoutSec: 单次调用超时秒数（0 = 默认 300s，对应重计算场景）
//	client:     可选 *http.Client（nil = 使用 SafeDialer 默认客户端）。
//	            调用方应传入 substrate.NewSafeHTTPClient() 以满足 XR-06 安全要求。
func NewRemoteSandbox(endpoint, authToken string, timeoutSec int, client *http.Client) *RemoteSandbox {
	if timeoutSec == 0 {
		timeoutSec = 300
	}
	if client == nil {
		client = substrate.NewSafeHTTPClient(nil)
	}
	client.Timeout = time.Duration(timeoutSec) * time.Second

	return &RemoteSandbox{
		endpoint:   endpoint,
		authToken:  authToken,
		httpClient: client,
	}
}

// Run 序列化 spec，POST 至 {endpoint}/execute，反序列化返回的 ToolResult。
func (s *RemoteSandbox) Run(ctx context.Context, spec SandboxSpec) (*protocol.ToolResult, error) {
	reqBody := RemoteExecRequest{
		ToolName:    spec.ToolName,
		Input:       spec.Input,
		Capability:  spec.Capability,
		SideEffects: spec.SideEffects,
		CPUQuotaMs:  spec.CPUQuotaMs,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "remote_sandbox: marshal request failed", err)
	}

	url := fmt.Sprintf("%s/execute", s.endpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "remote_sandbox: build request failed", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if s.authToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+s.authToken)
	}

	start := time.Now()
	resp, err := s.httpClient.Do(httpReq)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return &protocol.ToolResult{
			Success:   false,
			Error:     fmt.Sprintf("remote_sandbox: HTTP error: %v", err),
			LatencyMs: latencyMs,
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errText, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &protocol.ToolResult{
			Success:   false,
			Error:     fmt.Sprintf("remote_sandbox: status %d: %s", resp.StatusCode, errText),
			LatencyMs: latencyMs,
		}, nil
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024)) // 32MB 上限
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "remote_sandbox: read response failed", err)
	}

	var result protocol.ToolResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "remote_sandbox: unmarshal result failed", err)
	}
	result.LatencyMs = latencyMs
	return &result, nil
}
