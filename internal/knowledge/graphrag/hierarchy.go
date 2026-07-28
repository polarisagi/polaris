package graphrag

// HierarchicalSummarizer 支持两层摘要堆叠（Level-0 叶子 + Level-1 父社区）（GD-14-002）
type HierarchicalSummarizer struct {
	leafSummarizer *CommunityGenerativeSummarizer
}

// NewHierarchicalSummarizer 创建层次摘要器
func NewHierarchicalSummarizer(leaf *CommunityGenerativeSummarizer) *HierarchicalSummarizer {
	return &HierarchicalSummarizer{
		leafSummarizer: leaf,
	}
}

// BuildHierarchy 构建层次结构摘要占位
func (hs *HierarchicalSummarizer) BuildHierarchy(level0 []CommunitySummary, level1 []CommunitySummary) {
	// 预留层次合并逻辑
}
