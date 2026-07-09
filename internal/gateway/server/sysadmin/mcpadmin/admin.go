// Package mcpadmin 承载 MCP Server 管理（CRUD + 连接测试 + 网络访问审批）的
// HTTP handler，从 sysadmin 包摊平的 mcp_servers.go（原 411 行，R7 超标）拆出
// 为组合式子包（2026-07-07），沿用 cronadmin/insightsadmin/workflowadmin/
// channelsadmin 已验证过的模式：独立结构体 + 消费方定义的最小接口集 + 独立
// 构造函数，父 SysAdminHandler 只持有子结构体指针并做单行转发。
package mcpadmin

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
)

// InstallMgr mcpadmin 消费方视角的最小扩展安装授权接口。
type InstallMgr interface {
	Authorize(ctx context.Context, req protocol.ExtensionInstallRequest) error
	InstallExtension(ctx context.Context, req protocol.ExtensionInstallRequest) error
}

// MCPManager mcpadmin 消费方视角的最小 MCP 连接管理接口。
type MCPManager interface {
	ListServers() []protocol.MCPServerInfo
	Add(ctx context.Context, id, name string, cfg protocol.MCPClientConfig) error
	Update(ctx context.Context, extRepo protocol.ExtensionRepository, id string, cfg protocol.MCPUpdateConfig, dataDir string) error
	Remove(id string)
	ApproveNetworkAccess(ctx context.Context, id string, extRepo protocol.ExtensionRepository, dataDir string, approved bool) error
}

// SystemRepo mcpadmin 消费方视角的最小系统偏好读取接口。
type SystemRepo interface {
	ListPreferences(ctx context.Context) (map[string]string, error)
}

// MCPAdmin 承载 MCP Server CRUD + 连接测试 + 网络访问审批。
type MCPAdmin struct {
	DB         protocol.SQLQuerier
	MCPMgr     MCPManager
	SystemRepo SystemRepo
	InstallMgr InstallMgr
	ExtRepo    protocol.ExtensionRepository
	DataDir    string

	ClearToolSchemaCache func()
}

// NewMCPAdmin 构造 MCPAdmin。
func NewMCPAdmin(
	db protocol.SQLQuerier,
	mcpMgr MCPManager,
	systemRepo SystemRepo,
	installMgr InstallMgr,
	extRepo protocol.ExtensionRepository,
	dataDir string,
	clearToolSchemaCache func(),
) *MCPAdmin {
	return &MCPAdmin{
		DB:                   db,
		MCPMgr:               mcpMgr,
		SystemRepo:           systemRepo,
		InstallMgr:           installMgr,
		ExtRepo:              extRepo,
		DataDir:              dataDir,
		ClearToolSchemaCache: clearToolSchemaCache,
	}
}
