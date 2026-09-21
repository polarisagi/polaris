package main

import (
	"context"
	"strings"
	"testing"

	memstore "github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestProjectIsolation_AssemblerEpisodicAdapter 读取面 P5：Assembler 的情景检索适配器按
// 显式传入的项目过滤；空项目按默认项目（fail-closed）。由 tools/memory_isolation_check.go 纳入门控。
func TestProjectIsolation_AssemblerEpisodicAdapter(t *testing.T) {
	ctx := context.Background()
	ep := memstore.NewEpisodicMem(testutil.NewMockStore())
	for _, ev := range []types.Event{
		{ID: "a", ProjectID: "prj_a", Payload: []byte("ALPHA-MARKER 部署")},
		{ID: "b", ProjectID: "prj_b", Payload: []byte("BRAVO 部署")},
		{ID: "d", Payload: []byte("DEFAULT 部署")},
	} {
		if err := ep.Append(ctx, ev, types.TaintNone); err != nil {
			t.Fatal(err)
		}
	}
	a := &episodicMemAdapter{ep: ep}
	join := func(projectID string) string {
		items, err := a.Query(ctx, "部署", types.TaintHigh, projectID)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, it := range items {
			b.WriteString(it.Content)
		}
		return b.String()
	}
	if got := join("prj_b"); strings.Contains(got, "ALPHA") || !strings.Contains(got, "BRAVO") {
		t.Fatalf("项目 B 作用域错误: %q", got)
	}
	if got := join(""); strings.Contains(got, "ALPHA") || strings.Contains(got, "BRAVO") || !strings.Contains(got, "DEFAULT") {
		t.Fatalf("空项目应按默认项目: %q", got)
	}
}
