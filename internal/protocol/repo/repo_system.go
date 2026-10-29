package repo

import "context"

// SystemRepository 包含 preferences 与 kv_store 等通用配置表的读写契约。
type SystemRepository interface {
	GetPreference(ctx context.Context, key string) (string, error)
	ListPreferences(ctx context.Context) (map[string]string, error)
	UpsertPreference(ctx context.Context, key, value string) error
	DeletePreference(ctx context.Context, key string) error
	UpsertKV(ctx context.Context, key, value string) error
	RestoreKV(ctx context.Context, key, value, updatedAt string) error
	UpsertVFSRef(ctx context.Context, vfsURI string, blobSize int64, createdAt int64) error
}
