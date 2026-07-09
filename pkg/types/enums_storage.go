package types

// ============================================================================
// M2 Storage Fabric — 存储操作枚举
// 来源: internal/protocol/types.go §M2
//
// M5/M10 检索层枚举
// 来源: internal/protocol/types.go §M5/M10
//
// 从 enums.go 按模块拆出（R7 文件行数治理，2026-07-07），纯类型/常量声明，
// 无逻辑变更。
// ============================================================================

// OpType 定义批量写操作的类型。
type OpType int

const (
	OpPut OpType = iota
	OpDelete
)

// EvidenceType 标注检索结果的证据来源（HE-Rule-1 Surprise_Index）。
// Agent 可据此决策是否需要二次验证。
type EvidenceType string

const (
	EvidenceExactMatch   EvidenceType = "exact_match"   // 精确标题/关键字命中
	EvidenceHighVector   EvidenceType = "high_vector"   // 向量相似度 > 0.85
	EvidenceFTSKeyword   EvidenceType = "fts_keyword"   // BM25 全文检索命中
	EvidenceWeakSemantic EvidenceType = "weak_semantic" // 弱语义相似（向量 <= 0.85）
)
