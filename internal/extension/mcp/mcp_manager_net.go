package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// SetNetApprovalStore 注入网络访问审批存储（SystemRepo）。
// 必须在 RestoreServersFromDB / Add 之前调用；nil 表示跳过审批（默认断网）。
func (m *MCPManager) SetNetApprovalStore(s NetApprovalStore) {
	m.mu.Lock()
	m.netApproval = s
	m.mu.Unlock()
}

// netApprovalKey 生成 preferences 表的 key（mcp.net.approved.<serverID>）。
func netApprovalKey(serverID string) string {
	return "mcp.net.approved." + serverID
}

// checkNetApproval 查询指定 server 的网络访问审批状态。
// 返回 true 表示用户已批准；返回 false 表示 denied 或 pending（safe default）。
func (m *MCPManager) checkNetApproval(ctx context.Context, serverID string) bool {
	m.mu.RLock()
	store := m.netApproval
	m.mu.RUnlock()
	if store == nil {
		return false // 无存储则保守断网
	}
	val, err := store.GetPreference(ctx, netApprovalKey(serverID))
	if err != nil {
		slog.Warn("mcp: failed to read network approval state, defaulting to isolated",
			"server_id", serverID, "err", err)
		return false
	}
	return val == "approved"
}

// SetSamplingProvider 注入 LLM Provider，供 MCP sampling 回调使用。
func (m *MCPManager) SetSamplingProvider(p protocol.Provider) {
	m.mu.Lock()
	m.samplingProvider = p
	m.mu.Unlock()
}

// ApproveNetworkAccess 设置服务器的网络访问审批状态并立即重启该 MCP 连接，
// 使新的网络隔离策略立即生效（approved=true → 放行网络；false → 恢复断网）。
//
// 此方法：
//  1. 将 "approved"/"denied" 写入 preferences 表（持久化）。
//  2. 从 DB 读取最新配置（含 RequiresNetwork 字段）。
//  3. Remove + Add 重启连接（与 Update() 的重连模式相同）。
func (m *MCPManager) ApproveNetworkAccess(ctx context.Context, serverID string, extRepo protocol.ExtensionRepository, dataDir string, approved bool) error {
	m.mu.RLock()
	store := m.netApproval
	m.mu.RUnlock()
	if store == nil {
		return apperr.New(apperr.CodeInternal, "mcp: net approval store not configured")
	}

	// 1. 持久化审批结果
	decision := "denied"
	if approved {
		decision = "approved"
	}
	if err := store.UpsertPreference(ctx, netApprovalKey(serverID), decision); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: persist net approval", err)
	}
	slog.Info("mcp: network access decision recorded",
		"server_id", serverID, "decision", decision)

	// 2. 读取 DB 当前配置（重连时需要完整 row）
	row, err := extRepo.GetMCPServer(ctx, serverID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: get server for reconnect", err)
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "mcp: server not found: "+serverID)
	}
	if !row.Enabled {
		// 服务器当前禁用，仅存储决策，不触发重连
		return nil
	}

	// 3. 异步重启连接（与 Update 模式一致，不阻塞当前请求）
	var args []string
	var env map[string]string
	if err := json.Unmarshal([]byte(row.Args), &args); err != nil {
		args = nil
	}
	if err := json.Unmarshal([]byte(row.Env), &env); err != nil {
		env = nil
	}
	for i, a := range args {
		args[i] = strings.ReplaceAll(a, "{DATA_DIR}", dataDir)
	}
	transport := row.Transport
	if transport == "streamable-http" {
		transport = string(MCPStreamableHTTP)
	}
	clientCfg := MCPClientConfig{
		Transport:       MCPTransport(transport),
		Command:         row.Command,
		Args:            args,
		Env:             env,
		URL:             strings.ReplaceAll(row.URL, "{DATA_DIR}", dataDir),
		WorkDir:         row.WorkDir,
		Timeout:         time.Duration(row.Timeout) * time.Second,
		ServerName:      row.Name,
		TrustTier:       row.TrustTier,
		Trusted:         row.TrustTier >= 3,
		RequiresNetwork: row.RequiresNetwork,
		// NetworkApproved 由 Add() 内部重新查询 preferences，此处不预设
	}
	m.Remove(serverID)
	concurrent.SafeGo(context.Background(), "mcp_net_approve_reconnect", func(_ context.Context) {
		bgCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := m.Add(bgCtx, serverID, row.Name, clientCfg); err != nil {
			slog.Warn("mcp: reconnect after network approval failed", "server_id", serverID, "err", err)
		}
	})
	return nil
}

// MCPUpdateConfig protocol.MCPUpdateConfig 本地别名，使包内调用无需显式引用 protocol 包。
type MCPUpdateConfig = protocol.MCPUpdateConfig

// makeSamplingHandler 构建 MCP server 主动请求处理器，支持 sampling/createMessage 和 roots/list。
func (m *MCPManager) makeSamplingHandler() ServerRequestHandler {
	return func(ctx context.Context, method string, id int64, params json.RawMessage) (json.RawMessage, error) {
		switch method {
		case "sampling/createMessage":
			if m.samplingProvider == nil {
				return nil, apperr.New(apperr.CodeInternal, "sampling: no provider configured")
			}
			var req struct {
				Messages  []types.Message `json:"messages"`
				MaxTokens int             `json:"maxTokens"`
			}
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "sampling: invalid params", err)
			}
			opts := []types.InferOption{}
			if req.MaxTokens > 0 {
				opts = append(opts, types.WithMaxTokens(req.MaxTokens))
			}
			resp, err := safecall.Infer(ctx, m.samplingProvider, req.Messages, opts...)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "MCPManager.makeSamplingHandler", err)
			}
			result, _ := json.Marshal(map[string]any{
				"role":    "assistant",
				"content": map[string]any{"type": "text", "text": resp.Content},
				"model":   resp.Model,
			})
			return result, nil
		case "roots/list":
			// 返回空 roots 列表（当前不暴露文件系统 roots）
			result, _ := json.Marshal(map[string]any{"roots": []any{}})
			return result, nil
		default:
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("mcp: unsupported server method %q", method))
		}
	}
}
