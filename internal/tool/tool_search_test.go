package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/sandbox"
)

func TestToolSearch(t *testing.T) {
	toolReg := NewInMemoryToolRegistry(sandbox.NewExecEnvelope(nil, nil, 0, "", nil))
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
