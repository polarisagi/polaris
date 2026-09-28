package mcp

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ToolUI protocol.ToolUI 本地别名，使包内调用无需显式引用 protocol 包。
type ToolUI = protocol.ToolUI

// uiResourceURIScheme MCP Apps UI 资源 URI 的必需前缀（apps_spec.mdx §Content
// Requirements："URI MUST start with ui:// scheme"）。
const uiResourceURIScheme = "ui://"

const (
	// extensionUI 客户端能力声明 extensions 键名（io.modelcontextprotocol/ui，
	// apps_spec.mdx §Extension Identifier）。
	extensionUI = "io.modelcontextprotocol/ui"
	// mimeTypeMCPApp 标准 UI 资源 MIME type（apps_spec.mdx §UI Resource Format）。
	mimeTypeMCPApp = "text/html;profile=mcp-app"
	// mimeTypeSkybridge ChatGPT 旧 mime type 别名（openai_mcp_apps.md 历史命名，
	// window.openai 生态遗留服务器仍可能返回）。
	mimeTypeSkybridge = "text/html+skybridge"
)

// ParseToolUI 从工具 _meta 原文解析 MCP Apps UI 元数据（apps_spec.mdx §Resource
// Discovery + openai_mcp_apps.md ChatGPT 兼容别名表）。
//
// 优先级（标准字段优先，同时存在时以标准字段为准）：
//   - resourceUri: `_meta.ui.resourceUri` > 废弃别名 `_meta["ui/resourceUri"]`（记 Warn）
//     > ChatGPT 别名 `_meta["openai/outputTemplate"]`
//   - visibility: `_meta.ui.visibility` 显式声明时直接采用；否则若声明了
//     `_meta["openai/outputTemplate"]`，按 ChatGPT 语义取默认 ["model"]，
//     `_meta["openai/widgetAccessible"]==true` 时追加 "app"；两者都缺失时
//     visibility 留空（=规范默认 ["model","app"]，见 ToolUIVisibleTo）。
//
// resourceUri 非 "ui://" 前缀时整体丢弃 UI 元数据（工具本身仍可用，只是不再
// 关联 UI 视图；若同时声明了非默认 visibility 则保留 visibility 部分）并记 Warn。
// meta 为空或解析失败（非法 JSON）时返回 nil（无 UI 元数据，非错误——外部工具
// 定义不受信任，不能让一个格式错误的 _meta 拖垮整个工具注册）。
func ParseToolUI(meta json.RawMessage, toolName, serverName string) *ToolUI {
	if len(meta) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(meta, &raw); err != nil {
		slog.Warn("mcp apps: tool _meta is not a JSON object, ignoring", "server", serverName, "tool", toolName, "err", err)
		return nil
	}

	resourceURI, visibility := parseToolUIResourceURI(raw, toolName, serverName), parseToolUIVisibility(raw)

	if resourceURI != "" && !strings.HasPrefix(resourceURI, uiResourceURIScheme) {
		slog.Warn("mcp apps: tool ui.resourceUri must start with ui://, ignoring UI resource association",
			"server", serverName, "tool", toolName, "uri", resourceURI)
		resourceURI = ""
	}
	if resourceURI == "" && len(visibility) == 0 {
		return nil
	}
	return &ToolUI{ResourceURI: resourceURI, Visibility: visibility}
}

// parseToolUIResourceURI 见 ParseToolUI 顶部优先级说明。
func parseToolUIResourceURI(raw map[string]json.RawMessage, toolName, serverName string) string {
	if uiRaw, ok := raw["ui"]; ok {
		var ui struct {
			ResourceURI string `json:"resourceUri"`
		}
		if err := json.Unmarshal(uiRaw, &ui); err == nil && ui.ResourceURI != "" {
			return ui.ResourceURI
		}
	}
	if depRaw, ok := raw["ui/resourceUri"]; ok {
		var v string
		if err := json.Unmarshal(depRaw, &v); err == nil && v != "" {
			slog.Warn("mcp apps: deprecated _meta[\"ui/resourceUri\"] used, prefer _meta.ui.resourceUri",
				"server", serverName, "tool", toolName)
			return v
		}
	}
	if tplRaw, ok := raw["openai/outputTemplate"]; ok {
		var v string
		if err := json.Unmarshal(tplRaw, &v); err == nil && v != "" {
			return v
		}
	}
	return ""
}

// parseToolUIVisibility 见 ParseToolUI 顶部优先级说明。
func parseToolUIVisibility(raw map[string]json.RawMessage) []string {
	if uiRaw, ok := raw["ui"]; ok {
		var ui struct {
			Visibility []string `json:"visibility"`
		}
		if err := json.Unmarshal(uiRaw, &ui); err == nil && len(ui.Visibility) > 0 {
			return ui.Visibility
		}
	}
	_, hasOutputTemplate := raw["openai/outputTemplate"]
	widgetAccessible := false
	if waRaw, ok := raw["openai/widgetAccessible"]; ok {
		if err := json.Unmarshal(waRaw, &widgetAccessible); err != nil {
			// 非法值按 false 处理，不阻断解析（外部工具定义不受信任，见函数顶部注释）。
			widgetAccessible = false
		}
	}
	switch {
	case widgetAccessible:
		return []string{"model", "app"}
	case hasOutputTemplate:
		// ChatGPT 语义：声明 outputTemplate 但未显式 widgetAccessible 时，widget
		// 默认不可直接调用该工具（仅模型可见），与 MCP Apps 标准默认（两端均可见）不同。
		return []string{"model"}
	default:
		return nil
	}
}

// ToolUIVisibleTo 判定给定 UI 元数据在 target（"model" | "app"）视角下是否可见。
// nil 或空 Visibility 按规范默认视为 ["model","app"]（apps_spec.mdx §Visibility）。
func ToolUIVisibleTo(ui *ToolUI, target string) bool {
	if ui == nil || len(ui.Visibility) == 0 {
		return true
	}
	return slices.Contains(ui.Visibility, target)
}
