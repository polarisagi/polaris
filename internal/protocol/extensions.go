package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ============================================================================
// M7 Extensions — Plugin, Skill, MCP, Marketplace 模型
// ============================================================================

// RegistryEntry 插件目录条目（ADR-0016：Publisher/TrustTier/Type 字段）。
// ============================================================================

// FindPluginManifest 尝试在插件目录下寻找官方、Claude Code 或 Codex 的 manifest。
func FindPluginManifest(dir string) (string, error) {
	candidates := []string{
		filepath.Join(dir, ".polaris-plugin", "plugin.json"),
		filepath.Join(dir, ".claude-plugin", "plugin.json"),
		filepath.Join(dir, ".codex-plugin", "plugin.json"),
		filepath.Join(dir, "plugin.json"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// FindMCPConfig 尝试在插件目录下寻找标准的 MCP 配置文件。
func FindMCPConfig(dir string) (string, error) {
	candidates := []string{
		filepath.Join(dir, ".mcp.json"),
		filepath.Join(dir, "mcp.json"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

type RegistryEntry struct {
	// ID 全局唯一 slug，格式："{publisher}/{name}" 或 "mcp/{name}"
	ID        string `json:"id" yaml:"id"`
	Publisher string `json:"publisher" yaml:"publisher"`
	// Type "mcp" | "skill" | "plugin"
	Type      string `json:"type" yaml:"type"`
	TrustTier int    `json:"trust_tier" yaml:"trust_tier"`

	Name        string            `json:"name" yaml:"name"`
	Description string            `json:"description" yaml:"description"`
	Transport   string            `json:"transport,omitempty" yaml:"transport,omitempty"`
	Command     string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args        []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	URL         string            `json:"url,omitempty" yaml:"url,omitempty"`
	// Checksum 下载包的 SHA-256 校验值（hex 编码）；空字符串表示无校验（社区来源警告）。
	Checksum string `json:"checksum,omitempty" yaml:"checksum,omitempty"`
	// ChecksumURL 替代 Checksum 的远端校验文件 URL；优先级低于 Checksum。
	ChecksumURL string   `json:"checksum_url,omitempty" yaml:"checksum_url,omitempty"`
	Tags        []string `json:"tags" yaml:"tags"`
	Homepage    string   `json:"homepage,omitempty" yaml:"homepage,omitempty"`
	Timeout     int      `json:"timeout" yaml:"timeout"`
	// UI 展示元数据（来自 interface 块 或 agents/openai.yaml）
	DisplayName      string `json:"display_name,omitempty" yaml:"display_name,omitempty"`
	ShortDescription string `json:"short_description,omitempty" yaml:"short_description,omitempty"`
	Icon             string `json:"icon,omitempty" yaml:"icon,omitempty"`
	// 运行时叠加：是否已安装（extension_instances 表中存在同 catalog_id）
	Installed bool `json:"installed" yaml:"installed"`
	// 版本标识，若原数据无则自动填充为所在 repo 的 commit hash前缀
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	// 运行时叠加：本地已安装的版本标识
	InstalledVersion string `json:"installed_version,omitempty" yaml:"-"`
	// 运行时叠加：已安装实例 ID（原地升级接口用）与是否有新版本（目录版本 ≠ 已装版本，已装版本未知亦算）
	InstanceID      string `json:"instance_id,omitempty" yaml:"-"`
	UpdateAvailable bool   `json:"update_available,omitempty" yaml:"-"`
	// 运行时叠加：所属市场的排序权重（用于列表展示）
	MarketplaceSortOrder int `json:"marketplace_sort_order,omitempty" yaml:"-"`

	// ── 市场标准格式（ADR-0103 决策七）────────────────────────────────────────
	// MarketplaceName 市场清单 name（插件依赖按"同市场"解析）。
	MarketplaceName string `json:"marketplace_name,omitempty" yaml:"-"`
	// Entry 插件条目（pluginspec.MarketplaceEntry JSON）：来源与 strict 组合的唯一依据。
	Entry json.RawMessage `json:"entry,omitempty" yaml:"-"`
	// AllowCrossDeps 所属市场的 allowCrossMarketplaceDependenciesOn。
	AllowCrossDeps []string `json:"allow_cross_deps,omitempty" yaml:"-"`
	// SourceDir 技能条目在市场缓存中的目录（绝对路径）。
	SourceDir string `json:"source_dir,omitempty" yaml:"-"`

	// ── 目录覆盖层（ADR-0123）：网关读目录时按 catalog_id 运行时叠加，不进 payload ──────────
	// NameZh / SummaryZh / DescriptionZh 中文名称、一句话摘要、中文说明（可空）。
	NameZh        string `json:"name_zh,omitempty" yaml:"-"`
	SummaryZh     string `json:"summary_zh,omitempty" yaml:"-"`
	DescriptionZh string `json:"description_zh,omitempty" yaml:"-"`
	// Topics / Departments / Industries / Life 四维分类取值 ID（词表见目录接口返回的 taxonomy）。
	Topics      []string `json:"topics,omitempty" yaml:"-"`
	Departments []string `json:"departments,omitempty" yaml:"-"`
	Industries  []string `json:"industries,omitempty" yaml:"-"`
	Life        []string `json:"life,omitempty" yaml:"-"`
	// Featured 精选权重，越大越靠前；0 = 非精选。
	Featured int `json:"featured,omitempty" yaml:"-"`
	// I18nStale 上游文本已变而中文尚未重新生成：前端应以英文原文为主。
	I18nStale bool `json:"i18n_stale,omitempty" yaml:"-"`
}

// Marketplace 市场配置。
type Marketplace struct {
	ID          string `json:"id" yaml:"id"`
	Name        string `json:"name" yaml:"name"`
	Type        string `json:"type" yaml:"type"`
	Publisher   string `json:"publisher" yaml:"publisher"`
	RepoURL     string `json:"repo_url" yaml:"repo_url"`
	Description string `json:"description" yaml:"description"`
	IsBuiltin   int    `json:"is_builtin" yaml:"is_builtin"`
	TrustTier   int    `json:"trust_tier" yaml:"trust_tier"`
	Enabled     int    `json:"enabled" yaml:"enabled"`
	SortOrder   int    `json:"sort_order" yaml:"sort_order"` // 展示排序权重，值越小越靠前
	CreatedAt   string `json:"created_at" yaml:"created_at"`
}

// PluginInstallRequest 一键安装请求体。
type PluginInstallRequest struct {
	CatalogID string `json:"catalog_id"`
	// 可选覆盖：不传则使用 catalog 默认值
	Name    string            `json:"name,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

// ExtensionInstallRequest 描述一个扩展安装的请求。
// 用于 sysadmin 层与 marketplace 层间的解耦通信。
type ExtensionInstallRequest struct {
	Principal   string
	ExtensionID string // Instance ID (ext_...)
	CatalogID   string
	Name        string
	ExtType     string // plugin, skill, mcp
	TrustTier   int
	Publisher   string
	HasHooks    bool
	Target      any // Catalog 查找结果，installer 用它定位下载包
	Config      string
	BypassAuth  bool
	RuntimeID   string
	LocalPath   string
	// MarketplaceEntry 从市场安装插件时的条目（pluginspec.MarketplaceEntry JSON），按 strict 规则与插件清单组合。
	MarketplaceEntry json.RawMessage
}
