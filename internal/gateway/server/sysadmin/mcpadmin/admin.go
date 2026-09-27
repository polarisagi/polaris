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
	StartFromDB(ctx context.Context, id string) error
	Update(ctx context.Context, extRepo protocol.ExtensionRepository, id string, cfg protocol.MCPUpdateConfig, dataDir string) error
	Remove(id string)
	ApproveNetworkAccess(ctx context.Context, id string, extRepo protocol.ExtensionRepository, dataDir string, approved bool) error
	// BeginAuthorization 发起一次 MCP OAuth 授权流程，返回浏览器需跳转的授权 URL；
	// stdio 服务器返回 apperr.CodeInvalidInput（HandleAuthorizeMCPServer 借此映射 400）。
	BeginAuthorization(ctx context.Context, serverID, redirectBase string) (string, error)
}

// SystemRepo mcpadmin 消费方视角的最小系统偏好读取接口。
type SystemRepo interface {
	ListPreferences(ctx context.Context) (map[string]string, error)
}

// CredentialCipher mcpadmin 消费方视角的最小加密接口，仅用于 PUT .../oauth 预注册
// client_secret 落库前加密。解密留给 internal/extension/mcp 包自身在令牌交换/刷新时
// 使用——网关侧只写不读明文，因此不声明 Decrypt（HE-3：接口在调用方按需定义）。
type CredentialCipher interface {
	Encrypt(plaintext string) (string, error)
}

// MCPAdmin 承载 MCP Server CRUD + 连接测试 + 网络访问审批 + OAuth 网关接线。
type MCPAdmin struct {
	DB         protocol.SQLQuerier
	MCPMgr     MCPManager
	SystemRepo SystemRepo
	InstallMgr InstallMgr
	ExtRepo    protocol.ExtensionRepository
	DataDir    string
	// Cipher 加密 PUT .../oauth 预注册的 client_secret；nil 时该写入 fail-closed 拒绝
	// （不落明文，与 internal/extension/mcp 包令牌落库的 fail-closed 语义一致）。
	Cipher CredentialCipher

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
	cipher CredentialCipher,
) *MCPAdmin {
	return &MCPAdmin{
		DB:                   db,
		MCPMgr:               mcpMgr,
		SystemRepo:           systemRepo,
		InstallMgr:           installMgr,
		ExtRepo:              extRepo,
		DataDir:              dataDir,
		ClearToolSchemaCache: clearToolSchemaCache,
		Cipher:               cipher,
	}
}
