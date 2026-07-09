package protocol

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/pkg/types"
)

type

// Store 是所有存储引擎的统一接口。
// 引擎选择由 StorageRouter 路由规则决定，[Storage-SQLite] 兜底。
// 不同数据类型按访问模式路由到最匹配的引擎（向量/图/全文 → [Storage-SurrealDB-Core]，其余 → [Storage-SQLite]）。
Store interface {
	Get(ctx context.Context, key []byte) ([]byte, error)
	Put(ctx context.Context, key, value []byte) error
	Delete(ctx context.Context, key []byte) error
	Scan(ctx context.Context, prefix []byte) (Iterator, error)
	BatchWrite(ctx context.Context, ops []types.Op) error
	Txn(ctx context.Context, fn func(tx Transaction) error) error
	Capabilities() types.StoreCapabilities
	Close() error
}

type

// StoreExtStats 为底层存储引擎可选的统计扩展。
// 由具体 Store 实现提供，上层通过类型断言进行安全调用。
StoreExtStats interface {
	Stats() (string, error)
}

type

// StoreExtBackup 提供备份导入导出扩展。
StoreExtBackup interface {
	ImportBackupRow(ctx context.Context, table string, row map[string]any) error
}

type

// StoreExtPreferences 提供偏好设置的存储扩展。
StoreExtPreferences interface {
	ListPreferences(ctx context.Context) (map[string]string, error)
	SetPreference(ctx context.Context, key, value string) error
}

type

// StoreExtVector 为底层存储引擎可选的向量操作扩展。
StoreExtVector interface {
	VecSetMode(mode int) error
}

type

// TrajectoryStoreReader 提供近期行为轨迹的读取能力。
TrajectoryStoreReader interface {
	GetRecent(ctx context.Context, n int) ([]types.Trajectory, error)
}

type

// AuditLogger 提供审计日志记录能力。
AuditLogger interface {
	Log(ctx context.Context, action string, meta map[string]any) error
}

type

// SQLQuerier 是 *sql.DB 与 *sql.Tx 共同满足的最小 SQL 接口。
// 非存储层包（pkg/cognition/ pkg/swarm/ 等）必须接受此接口而非裸 *sql.DB，
// 以保持层边界。调用方构造时直接传入 *sql.DB，Go 结构化类型自动满足。
SQLQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type

// BlackboardDB 是 SQLiteBlackboard 所需的最小 *sql.DB 接口。
// 在 SQLQuerier 基础上扩展事务管理与健康检查，供 M8 多 Agent 协调层使用。
// *sql.DB 自动满足此接口（Go 结构化类型系统），调用方可直接传入 *sql.DB。
BlackboardDB interface {
	SQLQuerier
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
	PingContext(ctx context.Context) error
}

type

// ChatRepository 会话与消息的完整读写契约。
// @consumer: pkg/gateway/server/
// @producer: pkg/substrate/storage/repo_chat.go
ChatRepository interface {
	// Sessions
	CreateSession(ctx context.Context, row types.ChatSessionRow) error
	GetSession(ctx context.Context, id string) (*types.ChatSessionRow, error)
	ListSessions(ctx context.Context, limit int) ([]types.ChatSessionRow, error)
	UpdateSessionTitle(ctx context.Context, id, title string) error
	UpdateSessionThrashingIndex(ctx context.Context, id string, idx float64) error
	DeleteSession(ctx context.Context, id string) error

	// Messages
	AppendMessage(ctx context.Context, row types.ChatMessageRow) error
	ListMessages(ctx context.Context, sessionID string, limit int) ([]types.ChatMessageRow, error)
	SearchMessages(ctx context.Context, query string, limit int) ([]types.ChatMessageRow, error)

	// Additional mutations required by gateway/server
	RestoreSession(ctx context.Context, id, title string, thrashing float64, createdAt, updatedAt string) error
	RestoreMessage(ctx context.Context, id, sessionID, role, content, createdAt string) error
	TouchSession(ctx context.Context, id string) error
	ClearNonSystemMessages(ctx context.Context, sessionID string) error
	ReplaceSessionMessages(ctx context.Context, sessionID string, msgs []types.ChatMessageRow) error
}

type

// ProviderRepository Provider 配置读写契约。
// @consumer: pkg/gateway/server/, pkg/substrate/inference/
// @producer: pkg/substrate/storage/repo_provider.go
ProviderRepository interface {
	ListProviders(ctx context.Context) ([]types.ProviderRow, error)
	GetProvider(ctx context.Context, id string) (*types.ProviderRow, error)
	CreateProvider(ctx context.Context, p types.ProviderRow) error
	UpdateProvider(ctx context.Context, id string, p types.ProviderRow) error
	DeleteProvider(ctx context.Context, id string) error
	UpsertModel(ctx context.Context, row types.ProviderModelRow) error
	ListModels(ctx context.Context, providerID string) ([]types.ProviderModelRow, error)
	DeleteModel(ctx context.Context, id string) error
	DeleteModelsByProvider(ctx context.Context, providerID string) error

	ClearModelRoles(ctx context.Context, targetRoles []string, exceptID string) error
	SetModelRole(ctx context.Context, id string, role string) error
	// SeedIfEmpty 仅在 providers 表为空时插入默认配置；幂等。
	SeedIfEmpty(ctx context.Context, rows []types.ProviderRow, models []types.ProviderModelRow) error
	// SeedFromEnv 启动时根据环境变量插入或更新凭据。返回 (inserted_bool, error)
	SeedFromEnv(ctx context.Context, p types.ProviderRow) (bool, error)
	UpdateProviderAPIKey(ctx context.Context, id, apiKey, updatedAt string) error
	SeedModelFromEnv(ctx context.Context, m types.ProviderModelRow) error
}

type

// CronRepository 定时任务读写契约。
// @consumer: pkg/action/tool/cron_tools.go, pkg/gateway/server/ (cron.go)
// @producer: pkg/substrate/storage/repo_cron.go
CronRepository interface {
	ListCronJobs(ctx context.Context) ([]types.CronJobRow, error)
	GetCronJob(ctx context.Context, id string) (*types.CronJobRow, error)
	CreateCronJob(ctx context.Context, row types.CronJobRow) error
	UpdateCronJob(ctx context.Context, row types.CronJobRow) error
	DeleteCronJob(ctx context.Context, id string) error
	// UpdateCircuitBreaker 更新断路器状态（failure_count / circuit_open / last_error）。
	UpdateCircuitBreaker(ctx context.Context, id string, failureCount int, circuitOpen bool, lastError, circuitOpenedAt string) error
	// UpdateLastRun 更新最近执行时间与下次执行时间。
	UpdateLastRun(ctx context.Context, id, lastRunAt, nextRunAt string) error
}

type

// ExtensionRepository 扩展安装与目录读写契约。
// @consumer: pkg/extensions/native/, pkg/extensions/marketplace/, pkg/extensions/mcp/
// @producer: pkg/substrate/storage/repo_extension.go
ExtensionRepository interface {
	// extension_instances
	UpsertInstance(ctx context.Context, row types.ExtInstanceRow) error
	GetInstance(ctx context.Context, id string) (*types.ExtInstanceRow, error)
	UpdateInstanceStatus(ctx context.Context, id, status, errorMsg string) error
	UpdateInstanceInstallPath(ctx context.Context, id, installPath string) error
	ListInstances(ctx context.Context) ([]types.ExtInstanceRow, error)
	DeleteInstance(ctx context.Context, id string) error

	// extension_catalog
	GetCatalogEntry(ctx context.Context, id string) (*types.ExtCatalogRow, error)
	SearchCatalog(ctx context.Context, query string, limit int) ([]types.ExtCatalogRow, error)
	ListCatalogByIDs(ctx context.Context, ids []string) ([]types.ExtCatalogRow, error)
	ReplaceMarketplaceCatalog(ctx context.Context, marketplaceID string, entries []types.ExtCatalogRow) (int, error)
	DeleteOrphanCatalogEntries(ctx context.Context, activeMarketplaceIDs []any) error
	SeedMarketplace(ctx context.Context, row Marketplace) error
	CreateMarketplace(ctx context.Context, row Marketplace) error
	UpdateMarketplace(ctx context.Context, id string, row Marketplace) error
	UpdateMarketplaceSortOrder(ctx context.Context, id string, sortOrder int) error
	DeleteMarketplace(ctx context.Context, id string) (bool, error)
	GetMaxMarketplaceSortOrder(ctx context.Context) (int, error)
	SeedCatalogEntry(ctx context.Context, row types.ExtCatalogRow) error

	// apps
	UpsertApp(ctx context.Context, row types.AppRow) error
	DeleteApp(ctx context.Context, id string) error

	// plugins
	UpsertPlugin(ctx context.Context, id, name, version, displayName, description, publisher, homepage, installPath string, enabled, trustTier int, catalogID, mcpPolicy, manifest, createdAt, updatedAt string) error
	UpdatePluginStatus(ctx context.Context, id string, enabled int, mcpPolicy string, now string) error
	SetPluginComponentsEnabled(ctx context.Context, pluginID string, enabled int, now string) error
	UpdatePluginMCPServerEnabled(ctx context.Context, pluginID, serverID string, enabled int, now string) error
	// mcp_servers
	ListMCPServers(ctx context.Context) ([]types.MCPServerRow, error)
	GetMCPServer(ctx context.Context, id string) (*types.MCPServerRow, error)
	UpsertMCPServer(ctx context.Context, row types.MCPServerRow) error
	UpdateMCPServer(ctx context.Context, id string, fields map[string]any) error
	DeleteMCPServer(ctx context.Context, id string) error

	// UninstallCleanup 卸载扩展时清理关联数据（mcp_servers/skills/apps/plugins）
	UninstallCleanup(ctx context.Context, id, runtimeID, extType string) error
	// DeleteInstancesByPluginID 按 plugin_id 删除所有关联实例
	DeleteInstancesByPluginID(ctx context.Context, pluginID string) error
	// DeleteCatalogEntry 删除目录条目（非 builtin）
	DeleteCatalogEntry(ctx context.Context, id string) error
	// IsCatalogBuiltin 检查目录条目是否为内置
	IsCatalogBuiltin(ctx context.Context, id string) (bool, error)
}

type

// AuditRepository 审计日志读写契约。
// @consumer: pkg/substrate/security/audit_trail.go
// @producer: pkg/substrate/storage/repo_audit.go
AuditRepository interface {
	AppendAuditEvent(ctx context.Context, row types.AuditEventRow) error
	ListAuditEvents(ctx context.Context, limit int, before string) ([]types.AuditEventRow, error)
	DeleteAuditEventsBefore(ctx context.Context, before string) (int64, error)
}

type

// TaskReadRepository 任务表只读契约（写路径由 Blackboard 的 CAS 持有 *sql.DB）。
// @consumer: pkg/cognition/kernel/agent.go, pkg/edge/scheduler/cost_report.go
// @producer: pkg/substrate/storage/repo_task.go
TaskReadRepository interface {
	GetTaskProviderSuspendCount(ctx context.Context, taskID string) (int, error)
	GetTaskIntentTaint(ctx context.Context, taskID string) (int, error)
	AggregateTokenCosts(ctx context.Context, startISO, endISO string) ([]types.TokenCostAgg, error)
}
