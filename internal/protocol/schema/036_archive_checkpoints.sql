-- 归档检查点表：记录每次 EventArchiver 从 main.events 删除区间的最后一行 offset 与 hash，
-- 供跨库校验时拼接 cold_archive.events + main.events 两段链做完整性验证（GR-5-001）。
CREATE TABLE IF NOT EXISTS archive_checkpoints (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    archived_upto_offset INTEGER NOT NULL UNIQUE,  -- 被删除区间最后一行的 offset
    archived_upto_hash   TEXT    NOT NULL,          -- 被删除区间最后一行的 hash
    archived_at          INTEGER NOT NULL           -- 归档时间（Unix 毫秒）
);
