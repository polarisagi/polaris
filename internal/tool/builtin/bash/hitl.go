package bash

import "context"

type HITLGateway interface {
	RequestApproval(ctx context.Context, action string, req map[string]any) error
}

// Auditor 提供审计记录接口（可选，满足该接口的 HITLGateway 在 auto-approve 时写入审计，V9 / HE-1）。
type Auditor interface {
	Log(ctx context.Context, action string, meta map[string]any) error
}
