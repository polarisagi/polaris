package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
// sampling 防护参数（GD-14-002）：单次输出上限与每服务端每分钟 token 预算。
const (
	samplingMaxTokensPerCall  = 4096
	samplingDefaultMaxTokens  = 1024
	samplingTokenBudgetPerMin = 20000
)

// samplingBudget 每个 MCP server 一个的分钟级 token 预算（固定窗口）。
type samplingBudget struct {
	mu          sync.Mutex
	windowStart time.Time
	used        int
}

// reserve 预占 n 个 token；窗口内超额返回 false。
func (b *samplingBudget) reserve(n int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.windowStart) >= time.Minute {
		b.windowStart, b.used = now, 0
	}
	if b.used+n > samplingTokenBudgetPerMin {
		return false
	}
	b.used += n
	return true
}

// makeSamplingHandler 构造单个 MCP server 的反向请求处理器。
//
// GD-14-002：sampling/createMessage 让第三方服务端借用宿主 Provider 与额度推理。
// 原实现无任何门禁直接 Infer：任意已连接服务端都能烧光预算，并以 system 角色
// 向宿主模型注入指令。现在依次执行：PolicyGate（仅 TrustOfficial+ 放行）→
// 每服务端分钟级 token 预算 → 单次 max_tokens 封顶 → 消息角色降级（system→user，
// 服务端提供的一律按不可信用户数据处理）。
func (m *MCPManager) makeSamplingHandler(serverName string, trustTier int) ServerRequestHandler {
	budget := &samplingBudget{}
	return func(ctx context.Context, method string, id int64, params json.RawMessage) (json.RawMessage, error) {
		switch method {
		case "sampling/createMessage":
			if m.samplingProvider == nil {
				return nil, apperr.New(apperr.CodeInternal, "sampling: no provider configured")
			}
			if m.policy == nil {
				return nil, apperr.New(apperr.CodeForbidden, "sampling: policy gate not configured (fail-closed)")
			}
			allowed, pErr := m.policy.IsAuthorized(ctx, "mcp_mgr", "mcp_sampling", serverName,
				map[string]any{"trust_tier": trustTier})
			if pErr != nil || !allowed {
				return nil, apperr.New(apperr.CodeForbidden, fmt.Sprintf("sampling: denied by policy for server %q", serverName))
			}
			var req struct {
				Messages  []types.Message `json:"messages"`
				MaxTokens int             `json:"maxTokens"`
			}
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "sampling: invalid params", err)
			}
			maxTokens := req.MaxTokens
			if maxTokens <= 0 {
				maxTokens = samplingDefaultMaxTokens
			}
			maxTokens = min(maxTokens, samplingMaxTokensPerCall)
			if !budget.reserve(maxTokens, time.Now()) {
				return nil, apperr.New(apperr.CodeResourceExhausted, fmt.Sprintf("sampling: token budget exhausted for server %q", serverName))
			}
			msgs := make([]types.Message, 0, len(req.Messages))
			for _, msg := range req.Messages {
				if msg.Role != "assistant" {
					msg.Role = "user"
				}
				msgs = append(msgs, msg)
			}
			resp, err := safecall.Infer(ctx, m.samplingProvider, msgs, types.WithMaxTokens(maxTokens))
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
