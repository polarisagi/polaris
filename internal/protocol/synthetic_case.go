package protocol

// 合成评测用例跨模块契约（M09 §RAGAS Evolution）。
//
// producer: internal/learning/synthetic（EvalGenerator 具体生成实现，类型别名于此）
// consumer: internal/eval（SyntheticCaseToEvalCase 适配为 harness.EvalCase）
//
// SyntheticCase 此前以 internal/learning/synthetic.SyntheticCase 具体类型由
// internal/eval 直接 import 消费，违反 M04 §B2。现收敛至此。

// CaseSeverity 合成用例严重等级字符串（与 eval/harness.Severity 语义对齐）。
type CaseSeverity = string

// 合成用例严重等级常量。合成路径最高只能标 P2 —— P0/P1 只允许人工 incident 用例。
const (
	CaseSeverityP0 CaseSeverity = "P0" // 合成路径禁止使用：仅定义供比较
	CaseSeverityP1 CaseSeverity = "P1" // 合成路径禁止使用：仅定义供比较
	CaseSeverityP2 CaseSeverity = "P2" // 自动生成用例的上限等级
)

// QuestionType 问题类型分类（对齐 RAGAS / Giskard RAGET）。
type QuestionType string

const (
	QTypeFactual        QuestionType = "factual"        // 单跳，答案在单一 chunk
	QTypeMultiHop       QuestionType = "multi_hop"      // 需跨 2-3 chunk 聚合
	QTypeAbstractive    QuestionType = "abstractive"    // 需归纳/比较多文档
	QTypeCounterfactual QuestionType = "counterfactual" // 反事实，答案在语料中被否定
)

// DifficultyLevel 难度分级。
type DifficultyLevel string

const (
	DiffEasy   DifficultyLevel = "easy"   // 单跳，答案为连续 span
	DiffMedium DifficultyLevel = "medium" // 多跳或条件推理
	DiffHard   DifficultyLevel = "hard"   // 反事实 / 干扰项 / 抽象归纳
)

// SyntheticCase 合成评测用例，比 EvalCase 携带更多生成元数据。
type SyntheticCase struct {
	ID              string          `json:"id"`
	Question        string          `json:"question"`
	GroundTruth     string          `json:"ground_truth"`
	ChunkID         string          `json:"chunk_id"` // 来源 chunk 哈希
	Type            QuestionType    `json:"type"`
	Difficulty      DifficultyLevel `json:"difficulty"`
	ContextBound    bool            `json:"context_bound"`               // 仅凭 chunk 可回答（防污染）
	Severity        CaseSeverity    `json:"severity,omitempty"`          // P0/P1/P2；空默认 P2
	NeedsHumanAudit bool            `json:"needs_human_audit,omitempty"` // 高风险用例标记
}
