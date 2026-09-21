package repo

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// DefaultProjectID 默认项目 ID。schema 013_chat.sql 建表时种入，恒存在、不可删除；
// 存量会话 / channel 会话 / 无头会话（Cron/Workflow/Webhook）一律归属它（ADR-0097 决策一）。
const DefaultProjectID = types.DefaultProjectID

// ProjectRepository 项目（会话运行上下文容器）持久化契约。
// 关联表：013_chat.sql（projects、chat_sessions.project_id）。
// @consumer: internal/gateway/server/chat/（API）、internal/gateway/server/sysadmin/（备份恢复）、cmd/polaris/boot_agent.go
// @producer: internal/store/repo/repo_project.go
type ProjectRepository interface {
	// CreateProject 新建项目。ID 冲突返回 CodeAlreadyExists。
	CreateProject(ctx context.Context, p types.ProjectRow) error
	// GetProject 不存在返回 CodeNotFound；SessionCount 已填充。
	GetProject(ctx context.Context, id string) (*types.ProjectRow, error)
	// ListProjects 默认项目排最前，其后按 updated_at 倒序；SessionCount 已填充。
	ListProjects(ctx context.Context, includeArchived bool) ([]types.ProjectRow, error)
	// UpdateProject 整行更新可变字段（name/root_path/instructions/trusted/archived）。
	// 默认项目仅允许改 name（其余字段须保持空值/false，否则 CodeInvalidInput）。
	UpdateProject(ctx context.Context, p types.ProjectRow) error
	// DeleteProject 删除项目，其会话迁回默认项目（同事务）；删默认项目返回 CodeInvalidInput。
	DeleteProject(ctx context.Context, id string) error
	// GetProjectBySession 按会话反查所属项目；会话不存在返回 (nil, nil)。
	GetProjectBySession(ctx context.Context, sessionID string) (*types.ProjectRow, error)
	// RestoreProject 备份恢复用：按 ID upsert 并保留备份中的时间戳。
	// trusted 一律落 false——信任是用户在本机对某目录的显式授予，不随备份文件迁移
	// （否则一份外来备份即可让任意目录的上下文文件进入可信区，ADR-0097 决策二）。
	// 默认项目只恢复 name。
	RestoreProject(ctx context.Context, p types.ProjectRow) error
}
