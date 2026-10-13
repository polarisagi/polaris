package knowledge

import (
	"testing"
)

// 2026-07-14（ADR-0062）：NewHybridRetriever/NewHybridRetrieverWithEmbedder/
// NewHybridRetrieverWithGraph 三个构造函数随之删除——boot_knowledge.go 生产唯一
// 使用 NewHybridRetrieverWithCognitive（embedder/cognitive/graph 可传 nil 降级），
// 其余 3 个是从未被启动分级逻辑采纳的平行构造路径。
func TestHybridRetrieverConstructors(t *testing.T) {
	hr3 := NewHybridRetrieverWithCognitive(nil, nil, nil)
	if hr3 == nil {
		t.Errorf("NewHybridRetrieverWithCognitive returned nil")
	}
}
