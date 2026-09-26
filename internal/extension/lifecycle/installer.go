package lifecycle

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// Installer 扩展安装器接口（每个 ExtType 一个实现）。
type Installer interface {
	ExtType() types.ExtType
	// Install 完成运行时绑定；安装状态、安装路径与 runtime_id 由 InstallFSM 统一回写。
	Install(ctx context.Context, req InstallReq) (InstallResult, error)
	// Uninstall 执行类型专属卸载逻辑。
	Uninstall(ctx context.Context, req UninstallReq) error
}

type InstallReq struct {
	InstID    string
	CatalogID string
	Name      string
	Publisher string
	TrustTier int
	Target    any
	LocalPath string
	Config    string
}

// InstallResult 运行时绑定结果。RuntimeID 为运行时表主键（plugins.id / skills.name / mcp_servers.id）。
type InstallResult struct {
	Dir       string
	RuntimeID string
}

type UninstallReq struct {
	InstID    string
	RuntimeID string
	ExtType   types.ExtType
}
