// Package bus 提供扩展系统的统一调度总线。
// 所有扩展的安装/激活/调用通过此包协调，消除散落在各包的重复初始化逻辑。
//
// 设计原则：
//   - 薄协调层：不包含业务逻辑，只做路由和组合
//   - 依赖注入：所有子系统通过构造函数注入，不持有全局状态
//   - 单一权威：外部调用者只需了解 Bus 接口，无需知道内部分包
package bus

import (
	"context"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/extension/native"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ExtensionBus 扩展系统统一调度总线。
type ExtensionBus struct {
	installFSM *lifecycle.InstallFSM
	installMgr *marketplace.Manager
	activator  *native.ExtensionActivator
	extRepo    protocol.ExtensionRepository
}

func New(
	installFSM *lifecycle.InstallFSM,
	installMgr *marketplace.Manager,
	activator *native.ExtensionActivator,
	extRepo protocol.ExtensionRepository,
) *ExtensionBus {
	return &ExtensionBus{
		installFSM: installFSM,
		installMgr: installMgr,
		activator:  activator,
		extRepo:    extRepo,
	}
}

// Install 统一安装入口（ADR-0016 §硬约束 1）。
// 所有扩展类型（mcp/skill/plugin/app）必须经此入口，不得直接调用 InstallFSM。
func (b *ExtensionBus) Install(ctx context.Context, req marketplace.InstallRequest) error {
	return b.installMgr.InstallExtension(ctx, req)
}

// Activate 按需激活已安装扩展（语义搜索 + 动态连接）。
// 调用时机：Agent S_REPLAN 或 会话开始时。
func (b *ExtensionBus) Activate(ctx context.Context, goal string) ([]protocol.ActivatedHint, error) {
	hints, err := b.activator.FindAndActivate(ctx, goal)
	if err != nil {
		return nil, err
	}
	var res []protocol.ActivatedHint
	for _, h := range hints {
		res = append(res, protocol.ActivatedHint{
			ExtensionID: h.ExtensionID,
			ToolName:    h.ToolName,
			Description: h.Description,
		})
	}
	return res, nil
}

// ListInstalled 查询所有已安装扩展（带状态）。
func (b *ExtensionBus) ListInstalled(ctx context.Context) ([]types.ExtInstanceRow, error) {
	return b.extRepo.ListInstances(ctx)
}

// Uninstall 统一卸载入口（级联清理）。
func (b *ExtensionBus) Uninstall(ctx context.Context, catalogID string) error {
	return b.installMgr.UninstallExtension(ctx, catalogID)
}
