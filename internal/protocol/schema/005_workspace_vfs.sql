-- ============================================================================
-- 005_workspace_vfs: WorkspaceManager VFS 引用计数 + 系统配置
-- ============================================================================
-- 注意: schema_versions 表由 storage/store.go 在迁移运行前自动创建，不在此定义。
-- 关联: M2(Storage) §3, M7(Tool) §4.5
-- ============================================================================

-- VFS 文件引用计数：热行 >4KB 载荷外存，B-Tree 仅留 vfs_ref 指针
CREATE TABLE IF NOT EXISTS sys_vfs_references (
    vfs_ref    TEXT    PRIMARY KEY,  -- vfs://{sha256_prefix}/{uuid}.blob
    ref_count  INTEGER NOT NULL DEFAULT 1,
    blob_size  INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

-- 系统运行时配置：KV 存储（MigrationGuard 崩溃恢复 + 运行时动态配置）
-- 写入路径: SchemaManager（迁移阶段豁免直写）；运行时变更走 MutationBus
CREATE TABLE IF NOT EXISTS sys_config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
