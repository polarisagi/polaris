package immune

import (
	"context"
)

// ImmuneGateway provides M9 immune system scanning capabilities.
type ImmuneGateway interface {
	Scan(ctx context.Context, agentID string, scanType string) (any, error)
}
