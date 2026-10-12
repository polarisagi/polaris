package repo

// 跨节点委派（ADR-0111，048_mesh.sql）的行类型。时间一律 Unix 毫秒。

// MeshTokenRow mesh_peer_tokens 一行（不含摘要：摘要只在写入与按摘要查找时出现）。
type MeshTokenRow struct {
	TokenID      string
	PeerName     string
	PeerNodeID   string
	Scopes       string // 逗号分隔
	CreatedAtMs  int64
	ExpiresAtMs  int64
	RevokedAtMs  int64
	LastUsedAtMs int64
}

// MeshTaskRow mesh_peer_tasks 一行。
type MeshTaskRow struct {
	TaskID        string
	BBTaskID      string
	CallerNodeID  string
	CallerName    string
	TokenID       string
	Protocol      string
	ContextID     string
	MessageID     string
	TargetAgent   string
	SpawnDepth    int
	Chain         string // JSON 数组
	InputText     string
	State         string
	StatusMessage string
	Result        string
	Canceled      bool
	CreatedAtMs   int64
	UpdatedAtMs   int64
	ExpiresAtMs   int64
}

// MeshTaskFilter 委派任务查询条件（零值字段不参与过滤）。
type MeshTaskFilter struct {
	TaskID       string
	CallerNodeID string
	MessageID    string
	ContextID    string
	State        string
	ActiveOnly   bool // 只取 submitted / working / input_required
	// 键集分页：取 (created_at, task_id) 严格小于该游标的行，按两者倒序。
	BeforeCreatedMs int64
	BeforeTaskID    string
	OldestFirst     bool
	Limit           int
}

// MeshPeerRow mesh_peers 一行。
type MeshPeerRow struct {
	Name          string
	NodeID        string
	URL           string
	PublicKey     string
	IssuedTokenID string
	CreatedAtMs   int64
	LastSeenAtMs  int64
	LastError     string
}
