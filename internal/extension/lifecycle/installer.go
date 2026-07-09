package lifecycle

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// Installer 扩展安装器接口（每个 ExtType 一个实现）。
type Installer interface {
	ExtType() types.ExtType
	// 实现内部负责调用 extRepo.UpdateInstanceStatus("installed")。
	Install(ctx context.Context, req InstallReq) (installDir string, err error)
	// Uninstall 执行类型专属卸载逻辑。
	Uninstall(ctx context.Context, req UninstallReq) error
}

type InstallReq struct {
	InstID    string
	Name      string
	Publisher string
	TrustTier int
	Target    any
	LocalPath string
	Config    string
}

type UninstallReq struct {
	InstID    string
	RuntimeID string
	ExtType   types.ExtType
}
