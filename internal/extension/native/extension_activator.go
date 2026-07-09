package native

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
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
//   - embedFn 为 nil 时退化为纯 FTS（无向量召回）
//   - 激活失败单条跳过，不中断主流程
//   - 同一会话内已激活的扩展不重复连接（mcpMgr 内部幂等）
type ExtensionActivator struct {
	extRepo   protocol.ExtensionRepository
	cognitive CognitiveSearcher // SurrealDB 语义检索（来自 extension_manager.go 的接口）
	mcpMgr    *mcp.MCPManager   // 动态注册 MCP server
	embedFn   EmbedFn           // 可选：本地向量化函数（nil = 无 VecKNN 路径）
}

// NewExtensionActivator 构造激活器。
// cognitive/embedFn 均可为 nil（Tier-0 降级模式）。
func NewExtensionActivator(extRepo protocol.ExtensionRepository, cognitive CognitiveSearcher, mcpMgr *mcp.MCPManager, embedFn EmbedFn) *ExtensionActivator {
	return &ExtensionActivator{extRepo: extRepo, cognitive: cognitive, mcpMgr: mcpMgr, embedFn: embedFn}
}

// FindAndActivate 根据任务目标语义搜索已安装扩展，激活后返回工具提示列表。
// 调用时机：Agent S_REPLAN 分支（遇挫重规划时）。
//
// 搜索策略（FTS + VecKNN RRF 融合）：
//  1. FTSSearch(goal, 8) — 关键词召回基线（始终执行）
//  2. embedFn != nil → VecKNN(embed(goal), 8) — 语义向量召回
//  3. RRF(k=60) 合并两路结果，取 top-3 激活
//
// 任一步骤失败时降级：FTS 失败则退出；VecKNN 失败则跳过向量路径。
func (a *ExtensionActivator) FindAndActivate(ctx context.Context, goal string) ([]ActivatedToolHint, error) {
	if a.cognitive == nil || goal == "" {
		return nil, nil
	}

	// Step 1: FTS 基线（始终运行）
	ftsResults, err := a.cognitive.FTSSearch(goal, 8)
	if err != nil {
		slog.Warn("extension_activator: FTSSearch failed", "err", err)
		return nil, nil // FTS 失败：降级退出，不中断 replan
	}

	// Step 2: VecKNN 语义召回（embedFn 可用时）
	vecResults := a.tryVecKNN(ctx, goal)

	// Step 3: RRF 融合（两路均空则退出）
	merged := rrfMergeActivationResults(ftsResults, vecResults)
	if len(merged) == 0 {
		return nil, nil
	}

	// 激活 top-3（避免 Prompt 膨胀）
	topN := 3
	if len(merged) < topN {
		topN = len(merged)
	}
	var hints []ActivatedToolHint
	for _, r := range merged[:topN] {
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

// tryVecKNN 尝试向量化 goal 并执行 VecKNN 召回；任一步骤失败则返回 nil（降级）。
func (a *ExtensionActivator) tryVecKNN(ctx context.Context, goal string) []ScoredResult {
	if a.embedFn == nil {
		return nil
	}
	vec, err := a.embedFn(ctx, goal)
	if err != nil {
		slog.Warn("extension_activator: embed failed, skip VecKNN", "err", err)
		return nil
	}
	if len(vec) == 0 {
		return nil
	}
	results, err := a.cognitive.VecKNN(vec, 8)
	if err != nil {
		slog.Warn("extension_activator: VecKNN failed, skip", "err", err)
		return nil
	}
	return results
}

// rrfMergeActivationResults 对 FTS 和 VecKNN 两路结果执行 Reciprocal Rank Fusion（k=60）。
// 相同 ID 的结果 RRF 分数累加，最终按分数降序返回去重合并列表。
func rrfMergeActivationResults(fts, vec []ScoredResult) []ScoredResult {
	const k = 60
	scores := make(map[string]float64, len(fts)+len(vec))
	for rank, r := range fts {
		scores[r.ID] += 1.0 / float64(k+rank+1)
	}
	for rank, r := range vec {
		scores[r.ID] += 1.0 / float64(k+rank+1)
	}
	if len(scores) == 0 {
		return nil
	}
	result := make([]ScoredResult, 0, len(scores))
	for id, s := range scores {
		result = append(result, ScoredResult{ID: id, Score: s})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Score > result[j].Score })
	return result
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
	case "app":
		// App 是前端 UI 扩展，不注册工具，直接返回描述提示
		return &ActivatedToolHint{
			ExtensionID: extID,
			ToolName:    runtimeID,
			Description: snippet,
		}, nil
	default:
		slog.Warn("extension_activator: unknown ext_type, skip activation",
			"ext_id", extID, "ext_type", extType)
		return nil, nil // warn but don't block
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
