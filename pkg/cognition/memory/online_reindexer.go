package memory

import (
	"context"
	"database/sql"
	"encoding/binary"
	"log/slog"
	"math"
	"runtime"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// Embedder M1 embedding 服务接口（consumer-side，防包循环）。
// Embed 返回 float32 向量；ModelVersion 返回当前模型标识，
// 用于检测 episodic_events.embed_model_version 是否需要重建（inv_M5_03）。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	ModelVersion() string
}

// OnlineReindexer 后台批量更新 episodic_events.embedding + embed_model_version。
// 触发条件: embed_model_version = ”（未索引）或 != 当前版本（版本切换）。
// Tier 0 BM25+Simhash 路径不受影响——失败仅降级，不阻断检索（inv_M5_03）。
type OnlineReindexer struct {
	db        *sql.DB
	embedder  Embedder
	batchSize int
}

const defaultReindexBatchSize = 50

// NewOnlineReindexer 创建重建索引器。db 和 embedder 均必须非 nil。
func NewOnlineReindexer(db *sql.DB, embedder Embedder) *OnlineReindexer {
	return &OnlineReindexer{
		db:        db,
		embedder:  embedder,
		batchSize: defaultReindexBatchSize,
	}
}

// Run 执行一批重建索引。返回 (已处理数, 是否还有未索引条目, error)。
// 调用方在后台 goroutine 中循环调用，remaining=false 时停止。
// 单条失败不中断整批（best-effort，与 Consolidation Stage 2 原则一致）。
func (r *OnlineReindexer) Run(ctx context.Context) (processed int, remaining bool, err error) {
	version := r.embedder.ModelVersion()

	// idx_ep_embed_ver 偏索引（WHERE embed_model_version = ''）加速扫描
	rows, queryErr := r.db.QueryContext(ctx,
		`SELECT id, content FROM episodic_events
		 WHERE embed_model_version = '' OR embed_model_version != ?
		 LIMIT ?`,
		version, r.batchSize,
	)
	if queryErr != nil {
		return 0, false, perrors.Wrap(perrors.CodeInternal, "reindexer: query failed", queryErr)
	}
	defer rows.Close()

	type entry struct {
		id      int64
		content string
	}
	var batch []entry
	for rows.Next() {
		var e entry
		if scanErr := rows.Scan(&e.id, &e.content); scanErr == nil {
			batch = append(batch, e)
		}
	}
	if closeErr := rows.Close(); closeErr != nil {
		slog.Warn("reindexer: rows close error", "err", closeErr)
	}

	if len(batch) == 0 {
		return 0, false, nil
	}

	for _, e := range batch {
		vec, embedErr := r.embedder.Embed(ctx, e.content)
		if embedErr != nil {
			// Tier 0 BM25 路径兜底，单条失败仅告警继续
			slog.Warn("reindexer: embed failed, row skipped",
				"id", e.id, "err", embedErr)
			continue
		}
		if _, updateErr := r.db.ExecContext(ctx,
			`UPDATE episodic_events SET embedding = ?, embed_model_version = ? WHERE id = ?`,
			encodeFloat16(vec), version, e.id,
		); updateErr != nil {
			slog.Warn("reindexer: update failed", "id", e.id, "err", updateErr)
			continue
		}
		processed++
		runtime.Gosched() // 批内让出调度，避免长时间独占 goroutine
	}

	// 检查空版本条目（走 idx_ep_embed_ver 偏索引，O(1) 量级）
	// 仅检测 ''，版本切换场景由调用方决策是否重新触发，避免无限循环
	var cnt int
	_ = r.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM episodic_events WHERE embed_model_version = ''`,
	).Scan(&cnt)

	return processed, cnt > 0, nil
}

// encodeFloat16 将 float32 向量量化为 IEEE 754 half-precision BLOB（小端序）。
// 精度损失可接受：检索路径经 RRF 归一化，不依赖绝对精度（与 DDL 003/004 规范一致）。
func encodeFloat16(vec []float32) []byte {
	buf := make([]byte, len(vec)*2)
	for i, f := range vec {
		binary.LittleEndian.PutUint16(buf[i*2:], f32tof16(f))
	}
	return buf
}

// f32tof16 IEEE 754 float32 → float16 截断转换（round-toward-zero）。
func f32tof16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16((b >> 31) & 0x1)
	exp := int32((b>>23)&0xFF) - 127 + 15
	mant := uint16((b >> 13) & 0x3FF)
	switch {
	case exp <= 0:
		return sign << 15 // 下溢 → ±0
	case exp >= 31:
		return sign<<15 | 0x7C00 // 上溢 → ±∞
	default:
		return sign<<15 | uint16(exp)<<10 | mant
	}
}
