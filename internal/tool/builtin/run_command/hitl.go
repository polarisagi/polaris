package run_command

import "context"

type HITLGateway interface {
	RequestApproval(ctx context.Context, action string, req map[string]any) error
}
