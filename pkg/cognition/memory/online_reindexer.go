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
// 触发条件: embed_model_version = “（未索引）或 != 当前版本（版本切换）。
// Tier 0 BM25+Simhash 路径不受影响——失败仅降级，不阻断检索（inv_M5_03）。
type OnlineReindexer struct {
	db        *sql.DB
	embedder  Embedder
	batchSize int
	cognitive CognitiveSearcher // Tier1+：SurrealDB HNSW 写入，nil 时仅更新 SQLite BLOB
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

// NewOnlineReindexerWithCognitive 创建含 SurrealDB HNSW 写入的重建索引器（Tier1+）。
// 每批 embedding 计算完成后，同步写入 SurrealDB HNSW 向量索引（不影响 SQLite BLOB 路径）。
func NewOnlineReindexerWithCognitive(db *sql.DB, embedder Embedder, cognitive CognitiveSearcher) *OnlineReindexer {
	return &OnlineReindexer{
		db:        db,
		embedder:  embedder,
		batchSize: defaultReindexBatchSize,
		cognitive: cognitive,
	}
}

// Run 执行一批重建索引。返回 (已处理数, 是否还有未索引条目, error)。
// 调用方在后台 goroutine 中循环调用，remaining=false 时停止。
// 单条失败不中断整批（best-effort，与 Consolidation Stage 2 原则一致）。
func (r *OnlineReindexer) Run(ctx context.Context) (processed int, remaining bool, err error) {
	version := r.embedder.ModelVersion()

	// idx_ep_embed_ver 偏索引（WHERE embed_model_version = ''）加速扫描
	rows, queryErr := r.db.QueryContext(ctx,
		`SELECT id, event_uuid, content FROM episodic_events
		 WHERE (embed_model_version = '' OR embed_model_version != ?) AND cold = 0
		 LIMIT ?`,
		version, r.batchSize,
	)
	if queryErr != nil {
		return 0, false, perrors.Wrap(perrors.CodeInternal, "reindexer: query failed", queryErr)
	}
	defer rows.Close()

	type entry struct {
		id        int64
		eventUUID string // 原始 Event.ID（UUID），供 SurrealDB VecUpsert 使用；空时回退到整数串
		content   string
	}
	var batch []entry
	for rows.Next() {
		var e entry
		if scanErr := rows.Scan(&e.id, &e.eventUUID, &e.content); scanErr == nil {
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
		// SurrealDB HNSW 同步写入（Tier1+）；失败不阻断 SQLite BLOB 路径（Tier0 继续可用）
		if r.cognitive != nil {
			if e.eventUUID == "" {
				// event_uuid 为空（存量旧行）：整数串 docID 与 KV 键 "episodic:{uuid}" 不一致，
				// 跳过 VecUpsert 避免检索时 content 退化为 ID 字符串（BUG-2 修复）。
				// 此类行走 SQLite BLOB 余弦相似度降级路径，BM25/FTS 路径不受影响。
				slog.Debug("reindexer: skipping SurrealDB VecUpsert for legacy row without event_uuid",
					"id", e.id)
			} else if upsertErr := r.cognitive.VecUpsert(e.eventUUID, vec); upsertErr != nil {
				slog.Warn("reindexer: surreal vec_upsert failed, degrading to SQLite-only path",
					"id", e.id, "err", upsertErr)
			}
		}
		processed++
		runtime.Gosched() // 批内让出调度，避免长时间独占 goroutine
	}

	// 检查空版本条目（走 idx_ep_embed_ver 偏索引，O(1) 量级）
	// 仅检测 ''，版本切换场景由调用方决策是否重新触发，避免无限循环
	var cnt int
	_ = r.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM episodic_events WHERE embed_model_version = '' AND cold = 0`,
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

// DecodeFloat16 将 IEEE 754 half-precision BLOB 解析为 float32 向量。
func DecodeFloat16(blob []byte) []float32 {
	if len(blob)%2 != 0 {
		return nil
	}
	vec := make([]float32, len(blob)/2)
	for i := 0; i < len(vec); i++ {
		h := binary.LittleEndian.Uint16(blob[i*2:])
		vec[i] = f16tof32(h)
	}
	return vec
}

// f16tof32 IEEE 754 float16 → float32 转换。
func f16tof32(h uint16) float32 {
	sign := uint32((h >> 15) & 1)
	exp := uint32((h >> 10) & 0x1F)
	mant := uint32(h & 0x3FF)

	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign << 31)
		}
		// Subnormal
		for (mant & 0x400) == 0 {
			mant <<= 1
			exp--
		}
		exp++
		mant &= 0x3FF
		return math.Float32frombits((sign << 31) | ((exp + 127 - 15) << 23) | (mant << 13))
	case 31:
		if mant == 0 {
			return math.Float32frombits((sign << 31) | 0x7F800000) // ±∞
		}
		return math.Float32frombits((sign << 31) | 0x7F800000 | (mant << 13)) // NaN
	default:
		return math.Float32frombits((sign << 31) | ((exp + 127 - 15) << 23) | (mant << 13))
	}
}
