package repo

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
)

// AppRepository 提供了 M13 应用沙盒的数据库持久化支持。
// 关联表：028_apps.sql
type AppRepository interface {
	CreateApp(ctx context.Context, app *protocol.App) error
	GetApp(ctx context.Context, id string) (*protocol.App, error)
	ListApps(ctx context.Context, enabledOnly bool) ([]*protocol.App, error)
	UpdateApp(ctx context.Context, app *protocol.App) error
	DeleteApp(ctx context.Context, id string) error
	SetAppEnabled(ctx context.Context, id string, enabled bool) error
}
