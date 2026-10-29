package tool

import (
	"context"
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

	if len(out) == 0 {
		t.Fatal("expected text output")
	}
}
