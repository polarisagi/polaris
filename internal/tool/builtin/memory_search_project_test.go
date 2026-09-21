package builtin

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// scopeRecorder 记录 memory_search 传给检索器的作用域。
type scopeRecorder struct{ got []types.SearchScope }

func (r *scopeRecorder) Search(_ context.Context, _ string, scope types.SearchScope, _ types.RetrievalConfig) ([]types.ScoredFragment, error) {
	r.got = append(r.got, scope)
	return nil, nil
}

// TestProjectIsolation_MemorySearchToolScope 读取面 P6 的入口：工具从 agent 执行 ctx 取项目；
// 未注入（非 agent 调用）按默认项目，fail-closed。由 tools/memory_isolation_check.go 纳入门控。
func TestProjectIsolation_MemorySearchToolScope(t *testing.T) {
	rec := &scopeRecorder{}
	fn := MakeMemorySearchFn(rec)

	ctx := protocol.WithProjectID(context.Background(), "prj_b")
	if _, err := fn(ctx, []byte(`{"query":"部署"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := fn(ctx, []byte(`{"query":"部署","layer":"semantic"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := fn(context.Background(), []byte(`{"query":"部署"}`)); err != nil {
		t.Fatal(err)
	}
	want := []string{"prj_b", "prj_b", types.DefaultProjectID}
	if len(rec.got) != len(want) {
		t.Fatalf("调用次数 %d", len(rec.got))
	}
	for i, w := range want {
		if rec.got[i].ProjectID != w {
			t.Errorf("第 %d 次检索作用域 = %q, want %q", i, rec.got[i].ProjectID, w)
		}
	}
}
