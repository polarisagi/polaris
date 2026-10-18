package sleeptime

import (
	"context"
)

// Weakness 表示聚类提炼出的执行失败模式。
type Weakness struct {
	Signature    string   `json:"signature"`
	Purpose      string   `json:"purpose"`
	Category     string   `json:"category"`
	Count        int      `json:"count"`
	SampleErrors []string `json:"sample_errors"`
}

// ProposedChange 表示自改进模型提出的单一覆盖层文件修改提案。
type ProposedChange struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Reason  string `json:"reason"`
}

// LoopResult 自改进闭环单次运行的综合结果。
type LoopResult struct {
	Weaknesses       []Weakness       `json:"weaknesses"`
	Proposals        []ProposedChange `json:"proposals"`
	BaselineHeldIn   float64          `json:"baseline_held_in"`
	BaselineHeldOut  float64          `json:"baseline_held_out"`
	CandidateHeldIn  float64          `json:"candidate_held_in"`
	CandidateHeldOut float64          `json:"candidate_held_out"`
	Accepted         bool             `json:"accepted"`
	CommitHash       string           `json:"commit_hash,omitempty"`
	RejectionReason  string           `json:"rejection_reason,omitempty"`
}

// Evaluator 评测器接口，用于在 held-in (训练集) 与 held-out (验证集) 上评估覆盖层表现。
type Evaluator interface {
	// Evaluate 针对给定的覆盖层变动计算 held-in 与 held-out 分数（0.0 ~ 1.0）。
	// files 为 nil 时评估基线表现。
	Evaluate(ctx context.Context, overlayFiles map[string][]byte) (heldInScore, heldOutScore float64, err error)
}

// LLMProposer 提案生成器接口。
type LLMProposer interface {
	Propose(ctx context.Context, weaknesses []Weakness) ([]ProposedChange, error)
}

// offPeakGate 错峰窗口消费端接口。
type offPeakGate interface {
	Allow() bool
}

// AuditTrail 审计接口契约。
type AuditTrail interface {
	Record(ctx context.Context, actor, action, target string, metadata map[string]any) error
}
