package consolidation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// EventArchiver performs cold archiving of events older than EventlogWarmDays
// into a secondary attached SQLite database.
//
// Tier-0 磁盘水位门控（P1-3）：归档本身有 I/O/CPU 开销，磁盘空间充裕时没有
// 归档的紧迫性——2GB/8GB Tier-0 VPS 上不必要的周期性写放大是纯负担。Archive()
// 每次调用先探测 coldDBDir 所在文件系统的空闲占比，仅当低于 diskWatermarkPct
// （磁盘转紧张）才真正执行归档+删除；探针失败时 fail-open（视为"未知即不
// 施加门控"，退回无条件执行，避免探针故障导致归档永久停摆、历史数据无限
// 增长）。调用方（ConsolidationWorker 6h ticker，见 cmd/polaris/boot_tools.go）
// 保持不变，门控逻辑完全内聚在 Archive() 内部。
type EventArchiver struct {
	db               archiveDB
	warmDays         int
	coldDBDir        string
	diskWatermarkPct float64
	diskFreeRatioFn  func(path string) (float64, bool)
	// [Task 12] 行数和展开大小触发阈值。任意一个满足就触发，0 表示禁用该信号。
	hotRowLimit int64 // Hot 表行数阈值 (0=禁用)
	hotSizeMB   int64 // Hot 表 MB 大小阈值 (0=禁用)
}

// NewEventArchiver 构造 EventArchiver。
// diskWatermarkPct: 空闲磁盘占比阈值（0~100），低于此值才触发归档；<=0 表示
// 禁用门控（等价于旧版无条件归档，供不关心磁盘压力的部署或测试使用）。
// archiveDB 是 EventArchiver 所需的最小 DB 能力集。
//
// 2026-08-08：本字段原为 *sql.DB，违反 inv_NoRawSQLDBField（storage 层外禁持
// 具体连接）。protocol.SQLQuerier 不含 BeginTx，而冷热分层归档必须在事务里
// 完成「写冷库 + 删热库」，故按 HE-3「接口在调用方定义」就地声明最小集，
// 而不是去拓宽 protocol.SQLQuerier 让全仓陪跑一个多余方法。
type archiveDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func NewEventArchiver(db archiveDB, warmDays int, coldDBDir string, diskWatermarkPct float64) *EventArchiver {
	return &EventArchiver{
		db:               db,
		warmDays:         warmDays,
		coldDBDir:        coldDBDir,
		diskWatermarkPct: diskWatermarkPct,
		diskFreeRatioFn:  diskFreeRatio,
	}
}

// WithRowSizeLimits 注入行数和展开大小阈值参数（Task 12）。
// 这两个参数和 diskWatermarkPct 是"任意一个满足就触发"的关系。
func (ea *EventArchiver) WithRowSizeLimits(hotRowLimit, hotSizeMB int64) *EventArchiver {
	ea.hotRowLimit = hotRowLimit
	ea.hotSizeMB = hotSizeMB
	return ea
}

// Archive runs the archiving process. It attaches the cold DB, creates the events table
// if it doesn't exist, moves old events, and then detaches the cold DB.
// 磁盘水位充裕时直接跳过（返回 nil，不算错误），不执行 ATTACH/INSERT/DELETE。
func (ea *EventArchiver) Archive(ctx context.Context) error {
	if err := os.MkdirAll(ea.coldDBDir, 0755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EventArchiver.Archive: create cold db dir", err)
	}

	// 四信号任一触发归档：磁盘水位 OR 行数超限 OR 空间超限 OR 时间窗口（时间窗口在后续 SQL cutoff 处理）。
	// 拆分为独立方法以控制 nestif 复杂度。
	if shouldSkip, reason := ea.shouldSkipArchive(ctx); shouldSkip {
		slog.Info("polaris: event archiver skipped", "reason", reason)
		return nil
	}

	coldDBPath := filepath.Join(ea.coldDBDir, "events_archive.db")

	// 1. ATTACH DATABASE
	safePath := strings.ReplaceAll(coldDBPath, "'", "''")
	attachStmt := fmt.Sprintf("ATTACH DATABASE '%s' AS cold_archive", safePath)
	if _, err := ea.db.ExecContext(ctx, attachStmt); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EventArchiver.Archive: attach cold db", err)
	}
	defer func() {
		// L2：defer 内尽力而为，DETACH 失败不影响本次归档已完成的数据迁移，
		// 但会累积连接句柄（cold_archive 保持 ATTACH 状态），需要计数以便观测。
		if _, err := ea.db.ExecContext(context.Background(), "DETACH DATABASE cold_archive"); err != nil {
			slog.Warn("memory/event_archiver: DETACH DATABASE 失败，连接句柄可能累积", "err", err)
			metrics.GlobalMemoryColdArchiveDetachFailuresTotal.Add(1)
		}
	}()

	// 2. Create table in cold DB (mirroring the main events table)
	// We keep the exact same schema to ensure compatibility.
	createTableStmt := `
CREATE TABLE IF NOT EXISTS cold_archive.events (
    offset            INTEGER PRIMARY KEY AUTOINCREMENT,
    id                TEXT NOT NULL UNIQUE,
    topic             TEXT NOT NULL,
    actor             TEXT NOT NULL,
    type              TEXT NOT NULL,
    payload           BLOB NOT NULL,
    idempotency_key   TEXT UNIQUE,
    embedding         BLOB,
    memory_layer      TEXT DEFAULT 'episodic',
    salience          REAL DEFAULT 0.5,
    occurred_at       INTEGER,
    durative_group_id TEXT,
    created_at        INTEGER NOT NULL,
    metadata          TEXT,
    prev_hash         TEXT,
    hash              TEXT
);
	`
	if _, err := ea.db.ExecContext(ctx, createTableStmt); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EventArchiver.Archive: create cold archive table", err)
	}

	// 3. Calculate cutoff time in milliseconds
	cutoffMs := time.Now().AddDate(0, 0, -ea.warmDays).UnixMilli()

	// 4. Move data within a transaction
	tx, err := ea.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EventArchiver.Archive: begin tx", err)
	}
	defer func() {
		_ = tx.Rollback() // Safe to call after commit
	}()

	insertStmt := `
		INSERT INTO cold_archive.events (
			offset, id, topic, actor, type, payload, idempotency_key, embedding, 
			memory_layer, salience, occurred_at, durative_group_id, created_at, 
			metadata, prev_hash, hash
		)
		SELECT 
			offset, id, topic, actor, type, payload, idempotency_key, embedding, 
			memory_layer, salience, occurred_at, durative_group_id, created_at, 
			metadata, prev_hash, hash
		FROM main.events 
		WHERE created_at < ? AND NOT EXISTS (
			SELECT 1 FROM cold_archive.events c WHERE c.id = main.events.id
		)
	`
	if _, err := tx.ExecContext(ctx, insertStmt, cutoffMs); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EventArchiver.Archive: insert into cold db", err)
	}

	if err := recordArchiveCheckpoint(ctx, tx, cutoffMs); err != nil {
		return err
	}

	deleteStmt := `DELETE FROM main.events WHERE created_at < ?`
	res, err := tx.ExecContext(ctx, deleteStmt, cutoffMs)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EventArchiver.Archive: delete from main db", err)
	}

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "EventArchiver.Archive: commit tx", err)
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected > 0 {
		slog.Info("polaris: event archiver moved events to cold storage", "count", rowsAffected, "cutoff_ms", cutoffMs)
	}

	return nil
}

// shouldArchiveByRowOrSize 使用 SQLite dbstat 虚拟表近似估算 events 表的行数和页面占用。
// 避免全表 COUNT(*) 的高开销（Tier-0 2GB 环境中 COUNT(*) 可能扫全表）。
// 任意一个阈值触发就返回 true（该归档）；均未超限或阈值为 0 时返回 false。
// [Task 12] 四信号任一触发触发逻辑：此函数处理行数/大小两个信号。
func (ea *EventArchiver) shouldArchiveByRowOrSize(ctx context.Context) bool {
	if ea.hotRowLimit <= 0 && ea.hotSizeMB <= 0 {
		return false // 两个阈值均禁用，直接跳过查询
	}
	// dbstat 虚拟表：payload 列 = 每页字节数 * 页数（近似，不含索引页开销）
	// ncell = 该页的 cell 数，汇总后是近似行数（不含 overflow page 的多行分割）
	row := ea.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(ncell),0), COALESCE(SUM(payload),0) FROM dbstat WHERE name='events'`)
	var approxRows, approxBytes int64
	if err := row.Scan(&approxRows, &approxBytes); err != nil {
		// 查询失败时 fail-open：不阻断归档，视为"未超限"（继续依赖其他信号决策）
		slog.Warn("polaris: event archiver dbstat query failed", "err", err)
		return false
	}
	approxMB := approxBytes / (1024 * 1024)
	if ea.hotRowLimit > 0 && approxRows >= ea.hotRowLimit {
		slog.Info("polaris: event archiver triggered by hot row limit",
			"approx_rows", approxRows, "limit", ea.hotRowLimit)
		return true
	}
	if ea.hotSizeMB > 0 && approxMB >= ea.hotSizeMB {
		slog.Info("polaris: event archiver triggered by hot size limit",
			"approx_mb", approxMB, "limit_mb", ea.hotSizeMB)
		return true
	}
	return false
}

// shouldSkipArchive 综合判断四个信号是否应跳过归档。返回 (true, reason) 表示应跳过。
// 磁盘水位充裕 + 行数/大小均未超限 → 跳过；其他情况继续归档。
func (ea *EventArchiver) shouldSkipArchive(ctx context.Context) (bool, string) {
	if ea.diskWatermarkPct <= 0 || ea.diskFreeRatioFn == nil {
		// 无磁盘门控 → 不跳过（继续时间窗口归档）
		return false, ""
	}
	freeRatio, ok := ea.diskFreeRatioFn(ea.coldDBDir)
	if !ok {
		// 探针失败 fail-open：不跳过，退回无条件执行
		return false, ""
	}
	freePct := freeRatio * 100
	if freePct < ea.diskWatermarkPct {
		// 磁盘已紧张，触发归档
		slog.Info("polaris: event archiver triggered by disk watermark",
			"free_pct", freePct, "watermark_pct", ea.diskWatermarkPct)
		return false, ""
	}
	// 磁盘充裕——检查行数/大小是否超限
	if ea.shouldArchiveByRowOrSize(ctx) {
		slog.Info("polaris: event archiver triggered by row/size limit despite disk headroom",
			"free_pct", freePct)
		return false, ""
	}
	return true, "disk_headroom_sufficient_and_row_size_ok"
}

// recordArchiveCheckpoint 记录归档边界，供跨库哈希链完整性验证使用（GR-5-001）。
//
// 2026-08-08 改为失败即中止事务：此前 `_, _ =` 吞掉写入错误，而调用方紧接着
// 就会 DELETE 掉 main.events 里被归档的那批行。checkpoint 没写成而删除照常
// 提交，等于把冷热两库之间的哈希链锚点弄丢——链条断在哪里、断没断，事后无法
// 判定。HE-2 要求安全边界密码学可验证，这条记录就是那个可验证性的载体，不能
// "尽力而为"。查询本身失败（非 ErrNoRows）同理：拿不到边界就不该删。
func recordArchiveCheckpoint(ctx context.Context, tx *sql.Tx, cutoffMs int64) error {
	var lastOffset int64
	var lastHash sql.NullString
	const q = `SELECT "offset", hash FROM main.events WHERE created_at < ? ORDER BY "offset" DESC LIMIT 1`
	err := tx.QueryRowContext(ctx, q, cutoffMs).Scan(&lastOffset, &lastHash)
	if errors.Is(err, sql.ErrNoRows) {
		// cutoff 之前没有任何事件——无可归档，也就无边界可记，正常路径。
		return nil
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal,
			"EventArchiver.Archive: 归档边界查询失败，拒绝在无锚点的情况下删除热库事件", err)
	}
	if !lastHash.Valid {
		// 边界行存在但 hash 为 NULL。001_events.sql 的 hash 列可空（链首为
		// NULL，且存在绕过 MutationBus 直插的历史行），故这是合法状态而非故障，
		// 维持原语义：无锚点可记就不记，归档照常进行。但要留痕——若这行日志
		// 频繁出现，说明有写入路径没走哈希链，那是另一个需要查的问题。
		slog.WarnContext(ctx, "EventArchiver: 归档边界行 hash 为空，本次不记 checkpoint",
			"last_offset", lastOffset)
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO archive_checkpoints (archived_upto_offset, archived_upto_hash, archived_at) VALUES (?, ?, ?)`,
		lastOffset, lastHash.String, time.Now().UnixMilli(),
	); err != nil {
		return apperr.Wrap(apperr.CodeInternal,
			"EventArchiver.Archive: 归档边界 checkpoint 写入失败，拒绝在无锚点的情况下删除热库事件", err)
	}
	return nil
}
