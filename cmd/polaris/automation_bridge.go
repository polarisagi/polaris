package main

import (
	"context"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/builtin"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 编译期断言：sysadmin.AutomationFacade 结构性满足 builtin.AutomationService
var _ builtin.AutomationService = (*sysadmin.AutomationFacade)(nil)

// automationBridge 是 automation_* 内置工具与 sysadmin.AutomationFacade 之间的晚绑定：
// 工具在 bootTools 注册，而承载自动化管理的 Server 到 bootServer 才创建，两者之间经此桥接。
type automationBridge struct {
	svc atomic.Pointer[builtin.AutomationService]
}

func (b *automationBridge) Bind(s builtin.AutomationService) {
	b.svc.Store(&s)
}

func (b *automationBridge) get() (builtin.AutomationService, error) {
	p := b.svc.Load()
	if p == nil || *p == nil {
		return nil, apperr.New(apperr.CodeUnimplemented, "automation: 服务启动中")
	}
	return *p, nil
}

func (b *automationBridge) List(ctx context.Context) ([]protocol.AutomationSummary, error) {
	svc, err := b.get()
	if err != nil {
		return nil, err
	}
	res, err := svc.List(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "automationBridge.List", err)
	}
	return res, nil
}

func (b *automationBridge) SavePrompt(ctx context.Context, in protocol.PromptAutomationInput) (string, string, error) {
	svc, err := b.get()
	if err != nil {
		return "", "", err
	}
	id, nextRun, err := svc.SavePrompt(ctx, in)
	if err != nil {
		return "", "", apperr.Wrap(apperr.CodeOf(err), "automationBridge.SavePrompt", err)
	}
	return id, nextRun, nil
}

func (b *automationBridge) Delete(ctx context.Context, id string) error {
	svc, err := b.get()
	if err != nil {
		return err
	}
	if err := svc.Delete(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "automationBridge.Delete", err)
	}
	return nil
}

func (b *automationBridge) RunNow(ctx context.Context, id string) (string, error) {
	svc, err := b.get()
	if err != nil {
		return "", err
	}
	runID, err := svc.RunNow(ctx, id)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "automationBridge.RunNow", err)
	}
	return runID, nil
}

func (b *automationBridge) CountEnabledBySource(ctx context.Context, source string) (int, error) {
	svc, err := b.get()
	if err != nil {
		return 0, err
	}
	count, err := svc.CountEnabledBySource(ctx, source)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeOf(err), "automationBridge.CountEnabledBySource", err)
	}
	return count, nil
}
