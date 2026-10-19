package main

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/builtin"
)

// hitlGatewayWithAudit 包装 HITLGateway 并附加 protocol.AuditLogger，
// 供 bash 与 run_command 在 full 档自动放行时记录审计事件（V9 / HE-1）。
type hitlGatewayWithAudit struct {
	inner builtin.HITLGateway
	al    protocol.AuditLogger
}

func (h *hitlGatewayWithAudit) RequestApproval(ctx context.Context, action string, req map[string]any) error {
	if h.inner == nil {
		return nil
	}
	return h.inner.RequestApproval(ctx, action, req)
}

func (h *hitlGatewayWithAudit) Log(ctx context.Context, action string, meta map[string]any) error {
	if h.al != nil {
		return h.al.Log(ctx, action, meta)
	}
	return nil
}
