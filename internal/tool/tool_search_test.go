package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

type mockPolicy struct{}

func (mockPolicy) IsAuthorized(ctx context.Context, principal, action, resource string, contextData map[string]any) (bool, error) {
	return true, nil
}
func (mockPolicy) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

func TestToolSearch(t *testing.T) {
	toolReg := NewInMemoryToolRegistry(mockPolicy{})
	fn := MakeToolSearchFn(toolReg)
	ctx := context.Background()

	// invalid json
	_, err := fn(ctx, []byte(`{invalid`))
	if err == nil {
		t.Fatal("expected err")
	}

	// dummy search
	args := `{"query": "bash"}`
	out, err := fn(ctx, []byte(args))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json")
	}
}
