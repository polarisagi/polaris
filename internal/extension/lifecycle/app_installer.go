package lifecycle

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type AppInstaller struct {
	extRepo protocol.ExtensionRepository
}

func NewAppInstaller(extRepo protocol.ExtensionRepository) *AppInstaller {
	return &AppInstaller{
		extRepo: extRepo,
	}
}

func (a *AppInstaller) ExtType() types.ExtType { return types.TypeApp }

func (a *AppInstaller) Install(ctx context.Context, req InstallReq) (string, error) {
	// app 类型通常不需要运行时注册，只需标记为 installed
	// 在 InstallFSM 中统一处理 UpdateInstanceStatus("installed")
	return req.LocalPath, nil
}

func (a *AppInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	_ = a.extRepo.UninstallCleanup(ctx, "", req.RuntimeID, "app")
	return nil
}
