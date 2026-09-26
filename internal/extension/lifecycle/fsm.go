package lifecycle

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
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

func (f *InstallFSM) Install(ctx context.Context, req InstallReq, extType types.ExtType) (InstallResult, error) {
	installer, ok := f.installers[extType]
	if !ok {
		return InstallResult{}, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("install_fsm: no installer registered for ext_type=%q", extType))
	}

	res, err := installer.Install(ctx, req)
	if err != nil {
		_ = f.extRepo.UpdateInstanceStatus(ctx, req.InstID, "failed", err.Error())
		return res, apperr.Wrap(apperr.CodeOf(err), "install_fsm: Install 失败", err)
	}
	if res.Dir != "" {
		if err := f.extRepo.UpdateInstanceInstallPath(ctx, req.InstID, res.Dir); err != nil {
			return res, apperr.Wrap(apperr.CodeInternal, "install_fsm: 回写 install_path", err)
		}
	}
	if res.RuntimeID != "" {
		if err := f.extRepo.UpdateInstanceRuntimeID(ctx, req.InstID, res.RuntimeID); err != nil {
			return res, apperr.Wrap(apperr.CodeInternal, "install_fsm: 回写 runtime_id", err)
		}
	}
	if err := f.extRepo.UpdateInstanceStatus(ctx, req.InstID, "installed", ""); err != nil {
		return res, apperr.Wrap(apperr.CodeInternal, "install_fsm: 回写 installed", err)
	}
	return res, nil
}

// UninstallRuntime 实现 sandbox.RuntimeUninstaller：outbox 卸载处理器删除文件前调用。
func (f *InstallFSM) UninstallRuntime(ctx context.Context, extType, instanceID, runtimeID string) error {
	return f.Uninstall(ctx, UninstallReq{InstID: instanceID, RuntimeID: runtimeID, ExtType: types.ExtType(extType)})
}

func (f *InstallFSM) Uninstall(ctx context.Context, req UninstallReq) error {
	installer, ok := f.installers[req.ExtType]
	if ok {
		if err := installer.Uninstall(ctx, req); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "install_fsm: Uninstall 失败", err)
		}
	}
	return nil
}
