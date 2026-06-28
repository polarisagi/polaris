package types

// ExtType 扩展类型枚举（唯一权威，替代全局裸字符串 "plugin"/"mcp"/"skill"/"app"）。
type ExtType string

const (
	TypeMCP    ExtType = "mcp"
	TypePlugin ExtType = "plugin"
	TypeSkill  ExtType = "skill"
	TypeApp    ExtType = "app"
)

// Valid 返回是否为合法扩展类型。
func (t ExtType) Valid() bool {
	switch t {
	case TypeMCP, TypePlugin, TypeSkill, TypeApp:
		return true
	}
	return false
}

// NeedsDownload 返回该类型是否需要文件下载步骤。
// mcp / plugin 通过 mcp_servers / plugins 表 + 本地 runtime 管理，无需独立下载。
// skill / app 从 marketplace 下载 zip 包。
func (t ExtType) NeedsDownload() bool {
	return t == TypeSkill || t == TypeApp
}

// NeedsImmediateInstalled 返回安装后是否应立即标记 installed（无下载步骤的类型）。
func (t ExtType) NeedsImmediateInstalled() bool {
	return !t.NeedsDownload()
}
