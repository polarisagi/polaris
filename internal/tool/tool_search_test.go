package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/tool/catalog"
)

func TestToolSearch(t *testing.T) {
	memCat := catalog.NewMemoryCatalog()
	compCat := catalog.NewCompositeCatalog(memCat)
	fn := MakeToolSearchFn(compCat, nil)
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
