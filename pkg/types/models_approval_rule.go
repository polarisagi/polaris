package types

// ApprovalRuleRow 是一条项目级“以后不再询问”授权规则（表 approval_rules，ADR-0117）。
type ApprovalRuleRow struct {
	ID         string `json:"id"`
	ProjectID  string `json:"project_id"`
	Kind       string `json:"kind"` // cmd | domain | dir | app | tool
	Pattern    string `json:"pattern"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	HitCount   int    `json:"hit_count"`
}
