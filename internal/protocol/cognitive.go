package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// CognitiveStore represents a graph/vector hybrid database (like SurrealDB)
// used for advanced semantic and relational queries.
type CognitiveStore interface {
	// SearchTools finds relevant tools based on a query and graph context.
	SearchTools(ctx context.Context, query string, topK int) ([]types.ToolSchema, error)

	// AddToolNode inserts a tool into the cognitive graph.
	AddToolNode(ctx context.Context, tool types.ToolSchema) error

	// AddRelation adds a directed relationship between two nodes.
	AddRelation(ctx context.Context, fromID, relation, toID string) error
}
