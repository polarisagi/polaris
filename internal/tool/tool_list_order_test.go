package tool

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestList_DeterministicOrder 是 ADR-0105 决策一"字节稳定规则 1"的门控：工具名会渲染进系统提示词
// 稳定层，List() 若随 map 遍历序漂移，前缀缓存永不命中。注册 ≥20 个工具以高概率暴露乱序
// （Go map 迭代起点随机，25 个键连续两次同序的概率可忽略）。
func TestList_DeterministicOrder(t *testing.T) {
	reg := NewInMemoryToolRegistry(sandbox.NewExecEnvelope(nil, nil, 0, "", nil), config.DefaultThresholds().M7Tool)
	for i := 0; i < 25; i++ {
		// 故意非字典序注册。
		name := fmt.Sprintf("tool_%02d", (i*7)%25)
		if err := reg.Register(types.Tool{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	render := func() string {
		list := reg.List()
		names := make([]string, 0, len(list))
		for _, tl := range list {
			names = append(names, tl.Name)
		}
		return strings.Join(names, ",")
	}
	first := render()
	for i := 0; i < 50; i++ {
		if got := render(); got != first {
			t.Fatalf("List() 顺序不稳定:\n%s\n%s", first, got)
		}
	}
	names := strings.Split(first, ",")
	if !sort.StringsAreSorted(names) || len(names) != 25 {
		t.Fatalf("List() 应按名称升序返回全部 25 个工具: %v", names)
	}
}
