package lifecycle

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// InstallFSM 统一管理扩展的生命周期状态流转，根据 ExtType 分发给具体的 Installer。
type InstallFSM struct {
	installers map[types.ExtType]Installer
	extRepo    protocol.ExtensionRepository
}

func NewInstallFSM(extRepo protocol.ExtensionRepository) *InstallFSM {
	return &InstallFSM{
		installers: make(map[types.ExtType]Installer),
		extRepo:    extRepo,
	}
}

func (f *InstallFSM) RegisterInstaller(installer Installer) {
	f.installers[installer.ExtType()] = installer
}

func (f *InstallFSM) Install(ctx context.Context, req InstallReq, extType types.ExtType) (string, error) {
	installer, ok := f.installers[extType]
	if !ok {
		slog.Warn("InstallFSM: unknown ext_type, marking installed without runtime registration",
			"ext_type", extType, "inst_id", req.InstID)
		_ = f.extRepo.UpdateInstanceStatus(ctx, req.InstID, "installed", "")
		return req.LocalPath, nil
	}

	installDir, err := installer.Install(ctx, req)
	if err != nil {
		_ = f.extRepo.UpdateInstanceStatus(ctx, req.InstID, "failed", err.Error())
		return installDir, err
	}

	// 如果 installer 没有标记状态，统一标记
	_ = f.extRepo.UpdateInstanceStatus(ctx, req.InstID, "installed", "")
	return installDir, nil
}

func (f *InstallFSM) Uninstall(ctx context.Context, req UninstallReq) error {
	installer, ok := f.installers[req.ExtType]
	if ok {
		return installer.Uninstall(ctx, req)
	}
	return nil
}
