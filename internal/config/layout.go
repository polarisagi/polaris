package config

import (
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// DataLayout 定义运行时数据根目录下所有子目录的规范布局。
// 所有子系统必须从此结构取路径，禁止各自拼接 filepath.Join(dataDir, "xxx")。
// 新增子目录时同步更新 MkdirAll 列表。
type DataLayout struct {
	Root string // 例：~/.polarisagi/polaris

	// 顶层文件
	ConfigFile string // Root/config.toml

	// 子目录
	Data       string // Root/data        — SQLite + SurrealDB 数据库文件
	Logs       string // Root/logs        — 所有日志文件
	Config     string // Root/config      — Operator 配置覆盖、SOUL.md、prompts/
	Extensions string // Root/extensions  — 从 Marketplace 安装的插件（含内嵌 MCP）
	Skills     string // Root/skills      — 用户安装的技能脚本 / Wasm
	Models     string // Root/models      — AI 模型文件（SenseVoice 等）
	Workspace  string // Root/workspace   — Agent 每任务沙箱 VFS
	Sessions   string // Root/sessions    — 会话 transcript（旧名 transcripts）
	Audit      string // Root/audit       — 审计档案根目录
	Reports    string // Root/reports     — 月度成本报告
	Cache      string // Root/cache       — HTTP / 推理缓存
	Hooks      string // Root/hooks       — 用户事件 Hook 脚本
	Tmp        string // Root/tmp         — 临时下载 / 解压暂存

	// 派生路径（从上方字段组合，避免调用方再次拼接）
	SQLiteDB     string // Data/polaris.db
	SurrealDB    string // Data/surreal.db
	AuditArchive string // Audit/archive
	ConfigPrompt string // Config/prompts
	SkillSignKey string // Config/skill_signing.key
}

// NewDataLayout 返回以 root 为根的完整 DataLayout。
// overrides 中非空字段会覆盖对应子目录的默认派生路径（来自 DirsConfig）。
func NewDataLayout(root string, overrides DirsConfig) DataLayout {
	pick := func(override, defaultVal string) string {
		if override != "" {
			return expandHome(override)
		}
		return defaultVal
	}

	d := DataLayout{
		Root:       root,
		ConfigFile: filepath.Join(root, "config.toml"),
		Config:     filepath.Join(root, "config"),
		Extensions: filepath.Join(root, "extensions"),
		Skills:     filepath.Join(root, "skills"),
		Sessions:   filepath.Join(root, "sessions"),
		Audit:      filepath.Join(root, "audit"),
		Reports:    filepath.Join(root, "reports"),
		Cache:      filepath.Join(root, "cache"),
		Hooks:      filepath.Join(root, "hooks"),
		Tmp:        filepath.Join(root, "tmp"),
	}
	// 可覆盖的四个路径：logs、data（db）、workspace、models
	d.Logs = pick(overrides.LogsDir, filepath.Join(root, "logs"))
	d.Data = pick(overrides.DBDir, filepath.Join(root, "data"))
	d.Workspace = pick(overrides.WorkspaceDir, filepath.Join(root, "workspace"))
	d.Models = pick(overrides.ModelsDir, filepath.Join(root, "models"))

	// 派生路径从各自父目录计算
	d.SQLiteDB = filepath.Join(d.Data, "polaris.db")
	d.SurrealDB = filepath.Join(d.Data, "surreal.db")
	d.AuditArchive = filepath.Join(d.Audit, "archive")
	d.ConfigPrompt = filepath.Join(d.Config, "prompts")
	d.SkillSignKey = filepath.Join(d.Config, "skill_signing.key")
	return d
}

// expandHome 展开路径中的 ~ 前缀。
func expandHome(p string) string {
	if len(p) >= 2 && p[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// MkdirAll 创建所有运行时必要目录。启动时调用一次，幂等。
func (l DataLayout) MkdirAll() error {
	dirs := []string{
		l.Root,
		l.Data,
		l.Logs,
		l.Config,
		l.ConfigPrompt,
		l.Extensions,
		l.Skills,
		l.Models,
		l.Workspace,
		l.Sessions,
		l.Audit,
		l.AuditArchive,
		l.Reports,
		l.Cache,
		l.Hooks,
		l.Tmp,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "DataLayout.MkdirAll", err)
		}
	}
	return nil
}

// Migrate 将旧路径的文件/目录移动到规范位置。
// 幂等：目标已存在或源不存在时静默跳过。
// 上线前阶段调用即可；上线后存在生产数据时测试后再启用。
func (l DataLayout) Migrate() {
	type mv struct{ src, dst string }
	migrations := []mv{
		// 数据库：根目录 → data/
		{filepath.Join(l.Root, "polaris.db"), l.SQLiteDB},
		{filepath.Join(l.Root, "surreal_rust.db"), l.SurrealDB},
		// 日志：根目录 → logs/
		{filepath.Join(l.Root, "polaris.log"), filepath.Join(l.Logs, "polaris.log")},
		{filepath.Join(l.Root, "polaris.error.log"), filepath.Join(l.Logs, "polaris.error.log")},
		// 会话记录：transcripts/ → sessions/
		{filepath.Join(l.Root, "transcripts"), l.Sessions},
	}
	for _, m := range migrations {
		if _, err := os.Stat(m.dst); err == nil {
			continue // 目标已存在，跳过
		}
		if _, err := os.Stat(m.src); err != nil {
			continue // 源不存在，跳过
		}
		_ = os.Rename(m.src, m.dst)
	}
}
