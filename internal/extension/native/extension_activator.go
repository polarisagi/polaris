package native

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ActivatedToolHint 已激活扩展的工具描述，注入规划 Prompt。
type ActivatedToolHint struct {
	ExtensionID string // extension_instances.id
	ToolName    string // LLM 调用时使用的工具名
	Description string // 工具能力描述（来自 SurrealDB 司书摘要）
}

// ExtensionActivator 按需激活已安装扩展，返回可注入 Prompt 的工具提示。
// 设计原则：
//   - cognitive 为 nil 时静默退出（Tier-0 无 SurrealDB 时降级）
//   - 激活失败单条跳过，不中断主流程
//   - 同一会话内已激活的扩展不重复连接（mcpMgr 内部幂等）
type ExtensionActivator struct {
	extRepo   protocol.ExtensionRepository
	cognitive CognitiveSearcher // SurrealDB 语义检索（来自 extension_manager.go 的接口）
	mcpMgr    *mcp.MCPManager   // 动态注册 MCP server
}

// NewExtensionActivator 构造激活器。cognitive 可为 nil（降级模式）。
func NewExtensionActivator(extRepo protocol.ExtensionRepository, cognitive CognitiveSearcher, mcpMgr *mcp.MCPManager) *ExtensionActivator {
	return &ExtensionActivator{extRepo: extRepo, cognitive: cognitive, mcpMgr: mcpMgr}
}

// FindAndActivate 根据任务目标语义搜索已安装扩展，激活后返回工具提示列表。
// 调用时机：Agent S_REPLAN 分支（遇挫重规划时）。
// topK：最多激活 3 个扩展，避免 Prompt 膨胀。
func (a *ExtensionActivator) FindAndActivate(ctx context.Context, goal string) ([]ActivatedToolHint, error) {
	if a.cognitive == nil || goal == "" {
		return nil, nil
	}

	// Step 1: SurrealDB FTSSearch — 语义匹配已索引的扩展
	results, err := a.cognitive.FTSSearch(goal, 5)
	if err != nil {
		slog.Warn("extension_activator: FTSSearch failed", "err", err)
		return nil, nil // 降级：不中断 replan
	}
	if len(results) == 0 {
		return nil, nil
	}

	// 取 top-3 extension_id
	topN := 3
	if len(results) < topN {
		topN = len(results)
	}

	var hints []ActivatedToolHint
	for _, r := range results[:topN] {
		// ScoredResult 结构体仅有 ID 和 Score 字段
		// ID 形如 "ext_{extensionID}" 或者就是 extensionID
		extID := strings.TrimPrefix(r.ID, "ext_")
		hint, activateErr := a.activateOne(ctx, extID, "")
		if activateErr != nil {
			slog.Warn("extension_activator: activate failed", "ext_id", extID, "err", activateErr)
			continue
		}
		if hint != nil {
			hints = append(hints, *hint)
		}
	}
	return hints, nil
}

// activateOne 激活单个扩展，返回工具提示。
func (a *ExtensionActivator) activateOne(ctx context.Context, extID, snippet string) (*ActivatedToolHint, error) {
	inst, err := a.extRepo.GetInstance(ctx, extID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no rows") {
			return nil, nil // 未安装或已卸载，跳过
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "activateOne: query failed", err)
	}
	if inst.Status != "installed" {
		return nil, nil
	}

	// 信任分级门控（ADR-0016 §2.1 / HE-2 可验证执行）：
	// TrustUntrusted(0) 的扩展不允许激活——其来源未经任何签名或审核。
	// TrustLocal(1)+ 的扩展正常激活；trust_tier 透传至 MCP 连接请求供污点传播使用。
	if types.TrustTier(inst.TrustTier) == types.TrustUntrusted {
		slog.Warn("extension_activator: blocked untrusted extension",
			"ext_id", extID, "trust_tier", inst.TrustTier)
		return nil, apperr.New(apperr.CodeForbidden,
			"extension_activator: trust_tier=0 (Untrusted) blocks activation; install from a trusted source")
	}
	trustTier := types.TrustTier(inst.TrustTier)

	extType := inst.ExtType
	runtimeID := inst.RuntimeID
	config := inst.Config

	switch extType {
	case "mcp":
		return a.activateMCP(ctx, extID, runtimeID, config, snippet, trustTier)
	case "skill":
		// Skill 工具已在启动时通过 skills 表注册到 ToolRegistry，无需动态连接。
		// 只需将其描述注入 Prompt 告知 LLM 可用即可。
		return &ActivatedToolHint{
			ExtensionID: extID,
			ToolName:    runtimeID, // skills.name
			Description: snippet,
		}, nil
	case "plugin":
		// Plugin 是 TypeScript MCP server，与 MCP 激活路径相同。
		return a.activateMCP(ctx, extID, runtimeID, config, snippet, trustTier)
	default:
		return nil, nil
	}
}

// activateMCP 通过 MCPManager 动态连接一个 MCP server（幂等）。
func (a *ExtensionActivator) activateMCP(ctx context.Context, extID, runtimeID, configJSON, snippet string, trustTier types.TrustTier) (*ActivatedToolHint, error) {
	if a.mcpMgr == nil {
		return nil, nil
	}

	// 从 mcp_servers 表取连接配置（command/args/url/transport）
	mcpServer, err := a.extRepo.GetMCPServer(ctx, runtimeID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "activateMCP: query mcp_servers failed", err)
	}

	command := mcpServer.Command
	argsJSON := mcpServer.Args
	url := mcpServer.URL
	transport := mcpServer.Transport

	// 解析 args
	var args []string
	if argsJSON != "" && argsJSON != "null" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}

	// 动态连接（MCPManager 内部检查是否已连接，避免重复）
	slog.Debug("extension_activator: connecting MCP", "server", runtimeID, "trust_tier", trustTier)
	connectErr := a.mcpMgr.DynamicConnect(ctx, mcp.DynamicConnectRequest{
		ServerName: runtimeID,
		Transport:  transport, // "stdio" | "sse" | "http"
		Command:    command,
		Args:       args,
		URL:        url,
	})
	if connectErr != nil {
		slog.Warn("extension_activator: MCP connect failed", "server", runtimeID, "err", connectErr)
		return nil, nil // 连接失败不中断，跳过此扩展
	}

	return &ActivatedToolHint{
		ExtensionID: extID,
		ToolName:    "mcp__" + runtimeID, // 与 MCPToolName 前缀对齐
		Description: snippet,
	}, nil
}
