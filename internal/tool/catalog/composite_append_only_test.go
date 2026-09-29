package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func schemaNames(ss []types.ToolSchema) string {
	names := make([]string, len(ss))
	for i, s := range ss {
		names[i] = s.Name
	}
	return strings.Join(names, ",")
}

func lazyCatalog() *CompositeCatalog {
	src := &fakeSource{entries: []protocol.CatalogEntry{
		coreEntry("core_b"), coreEntry("core_a"),
		communityEntry("tool_a"), communityEntry("tool_b"), communityEntry("tool_c"),
	}}
	cc := NewCompositeCatalog(src)
	cc.LazyLoadThreshold = 1
	cc.Embedder = fakeEmbedder{}
	return cc
}

func sessionCtx(id string) context.Context {
	return context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, id)
}

// ADR-0105 决策三：先激活 B 再激活 A，输出为 [核心..., search_tools, B, A]，核心部分字节不变。
func TestSchemas_LazyLoad_AppendOnlyInActivationOrder(t *testing.T) {
	cc := lazyCatalog()
	ctx := sessionCtx("s1")

	base := cc.Schemas(ctx, types.TrustUntrusted)
	if got := schemaNames(base); got != "core_a,core_b,search_tools" {
		t.Fatalf("初始应为核心(按名称)+search_tools：%s", got)
	}
	baseJSON, _ := json.Marshal(base)

	cc.ActivateTool("s1", "tool_b")
	afterB := cc.Schemas(ctx, types.TrustUntrusted)
	cc.ActivateTool("s1", "tool_a")
	afterA := cc.Schemas(ctx, types.TrustUntrusted)

	if got := schemaNames(afterA); got != "core_a,core_b,search_tools,tool_b,tool_a" {
		t.Fatalf("激活部分应按激活顺序 B,A 追加：%s", got)
	}
	// 每次激活只在尾部追加：前一次的完整输出是后一次的字节前缀。
	prefix := func(shorter, longer []types.ToolSchema) bool {
		a, _ := json.Marshal(shorter)
		b, _ := json.Marshal(longer)
		return strings.HasPrefix(string(b), strings.TrimSuffix(string(a), "]"))
	}
	if !prefix(base, afterB) || !prefix(afterB, afterA) {
		t.Fatal("激活必须只追加，不得改动已有 tools 的字节")
	}
	// 核心部分（前 3 项）字节不变。
	head, _ := json.Marshal(afterA[:3])
	if string(head) != string(baseJSON) {
		t.Fatal("核心部分字节必须不随激活变化")
	}
}

// 重复激活不改变位置；核心工具被"激活"不重复出现；未知/被过滤的激活项静默跳过。
func TestSchemas_LazyLoad_ReactivationAndEdgeCases(t *testing.T) {
	cc := lazyCatalog()
	ctx := sessionCtx("s2")
	cc.ActivateTool("s2", "tool_c")
	cc.ActivateTool("s2", "tool_a")
	cc.ActivateTool("s2", "tool_c") // 重复
	cc.ActivateTool("s2", "core_a") // 核心，已在前段
	cc.ActivateTool("s2", "ghost")  // 不存在
	if got := schemaNames(cc.Schemas(ctx, types.TrustUntrusted)); got != "core_a,core_b,search_tools,tool_c,tool_a" {
		t.Fatalf("got %s", got)
	}
	// 会话隔离。
	if got := schemaNames(cc.Schemas(sessionCtx("other"), types.TrustUntrusted)); got != "core_a,core_b,search_tools" {
		t.Fatalf("其他会话不应看到激活项：%s", got)
	}
}

// 非懒加载模式保持 List 的确定序（来源权重 + 名称），不受激活影响。
func TestSchemas_NonLazy_KeepsSortedOrder(t *testing.T) {
	cc := lazyCatalog()
	cc.LazyLoadThreshold = 100
	cc.ActivateTool("s3", "tool_c")
	got := schemaNames(cc.Schemas(sessionCtx("s3"), types.TrustUntrusted))
	if got != "core_a,core_b,tool_a,tool_b,tool_c" {
		t.Fatalf("got %s", got)
	}
}

// 单会话激活数有界：超限淘汰最早激活项。
func TestActivateTool_BoundedPerSession(t *testing.T) {
	var entries []protocol.CatalogEntry
	entries = append(entries, coreEntry("core"))
	for i := 0; i < maxActivatedToolsPerSession+5; i++ {
		entries = append(entries, communityEntry(fmt.Sprintf("t%03d", i)))
	}
	cc := NewCompositeCatalog(&fakeSource{entries: entries})
	cc.LazyLoadThreshold = 1
	cc.Embedder = fakeEmbedder{}
	for i := 0; i < maxActivatedToolsPerSession+5; i++ {
		cc.ActivateTool("s", fmt.Sprintf("t%03d", i))
	}
	got := cc.Schemas(sessionCtx("s"), types.TrustUntrusted)
	if len(got) != 2+maxActivatedToolsPerSession { // core + search_tools + 上限
		t.Fatalf("激活数应被限制在 %d，got %d", maxActivatedToolsPerSession, len(got)-2)
	}
	if got[2].Name != "t005" {
		t.Fatalf("应淘汰最早的 5 个，首个应为 t005：%s", got[2].Name)
	}
}

// 并发激活与读取无数据竞争（配合 -race）。
func TestSchemas_ConcurrentActivateAndRead(t *testing.T) {
	cc := lazyCatalog()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for _, n := range []string{"tool_a", "tool_b", "tool_c"} {
				cc.ActivateTool("s", n)
			}
		}()
		go func() {
			defer wg.Done()
			_ = cc.Schemas(sessionCtx("s"), types.TrustUntrusted)
		}()
	}
	wg.Wait()
}
