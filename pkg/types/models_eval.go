package types

type

// VerificationResult 验证阶段的结构化产出。
VerificationResult struct {
	Verdict  VerificationVerdict
	Findings []VerificationFinding
	// Summary 为 Verifier Agent 的整体评述（可作为下一轮重规划的输入）。
	Summary string
}

type

// VerificationFinding 单条验证发现。
VerificationFinding struct {
	Verdict     VerificationVerdict
	Description string
	// EvidencePath 指向支撑此发现的代码/文件路径（可选）。
	EvidencePath string
}

type

// StagingCandidate 自改善候选提交载荷（7 阶段 Staging 流水线入参）。
StagingCandidate struct {
	Type           string // skill / lora / prompt / config / source_patch / user_preference
	EvolutionLevel string // Evo-L0..L4
	SourceWorker   string
	PayloadPath    string
}

type

// EvalRunReport Eval Suite 运行报告。
EvalRunReport struct {
	Suite      string `json:"suite"`
	TotalCases int    `json:"total_cases"`
	PassCount  int    `json:"pass_count"`
	FailCount  int    `json:"fail_count"`
	P0Fail     int    `json:"p0_fail"`
	P1Fail     int    `json:"p1_fail"`
	P0Count    int    `json:"p0_count"`
	SafetyFail int    `json:"safety_fail"` // 一票否决计数
	// SkippedLowFalsifiability 是因 FalsifiabilityScore < 阈值而跳过 L4 评分的用例数（Gap-B）。
	SkippedLowFalsifiability int    `json:"skipped_low_falsifiability,omitempty"`
	Status                   string `json:"status"`
}

type

// ReplayReport 重放一致性报告（g_inv_08: 重放不得触发新 LLM 调用）。
ReplayReport struct {
	SessionID       string
	Consistent      bool
	DivergentOffset int64
	NewLLMCalls     int // 必须为零（g_inv_08）
}
