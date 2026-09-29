package repo

// HITL 审批请求的 status / decided_by 取值（044_hitl_requests.sql CHECK 约束的镜像）。
const (
	HITLStatusPending  = "pending"
	HITLStatusApproved = "approved"
	HITLStatusDenied   = "denied"
	HITLStatusTimeout  = "timeout"
	HITLStatusOrphaned = "orphaned"

	HITLByHuman          = "human"
	HITLByAutoApprove    = "auto_approve"
	HITLByAutoDeny       = "auto_deny"
	HITLByTrustDowngrade = "trust_downgrade"
	HITLByTimeoutKill    = "timeout_kill"
	HITLByL3Gate         = "l3_gate"
)

// HITLRequestRow hitl_requests 一行。DecidedBy 为空串表示 NULL（未决）；
// DecidedAtMs 为 0 表示 NULL。
type HITLRequestRow struct {
	ID             string
	AgentID        string
	SessionID      string
	CheckpointType string
	RiskLevel      int
	TaintLevel     int
	PromptJSON     string
	Status         string
	DecidedBy      string
	Reason         string
	ResponseJSON   string
	DeadlineNs     int64
	CreatedAtMs    int64
	DecidedAtMs    int64
}
