package knowledge

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ADR-0113 决策一：知识库向量由 KnowledgeReindexer 单一路径写入。
// 形态对齐 memory/retrieval.OnlineReindexer（扫描 → 关闭 Rows → 嵌入 → VecUpsert → 写回版本）。

const (
	// knowledgeReindexBatchSize 单批取出的待索引块数（SSoT；与 OnlineReindexer.defaultReindexBatchSize 同值）。
	knowledgeReindexBatchSize = 50
	// KnowledgeReindexInterval 兜底定时唤醒间隔（SSoT；与 cmd/polaris OnlineReindexer 的 5min tick 同值）。
	// Kick() 负责摄入后的即时唤醒，tick 负责换模型 / 失败重试等无事件触发的收敛。
	KnowledgeReindexInterval = 5 * time.Minute
	// knowledgeReindexMaxConsecutiveEmbedFails 单批内连续嵌入失败达到该值即中止本批（同 ADR-0109 D5）。
	knowledgeReindexMaxConsecutiveEmbedFails = 3
	// knowledgeReindexMaxBatchesPerRound 单轮最多连续处理的批数，防止积压极大时独占后台额度。
	knowledgeReindexMaxBatchesPerRound = 200
)

// ChunkEmbedder 知识重嵌器所需的嵌入器（consumer-side，防包循环）。
// ModelVersion 为 "" 表示引擎未就绪；其唯一来源是 DynamicEmbedder（ADR-0109）。
type ChunkEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	ModelVersion() string
}

// VectorIndexWriter 知识向量的写入端（SurrealDB HNSW，consumer-side）。
type VectorIndexWriter interface {
	VecUpsert(id string, vec []float32) error
}

// KnowledgeReindexer 后台把 rag_chunks 嵌入并写入向量索引，随后写回 embed_model_version。
// 待索引判据：deleted_at IS NULL AND embed_model_version != 当前嵌入模型版本
// （空串即从未索引，换模型后旧版本同样不等）。失败行保持旧版本，下一轮重试。
type KnowledgeReindexer struct {
	db       protocol.SQLQuerier
	embedder ChunkEmbedder
	index    VectorIndexWriter

	batchSize int
	kick      chan struct{} // 容量 1；Kick 非阻塞

	// gate 可选：每轮前的资源准入，返回 release 归还额度；ok=false 跳过本轮。
	gate func() (release func(), ok bool)
	// onResult 可选：每轮结果回调（成功 err=nil），供调用方接退避器。
	onResult func(err error)
}

// NewKnowledgeReindexer 创建重嵌器。db / embedder / index 均须非 nil。
func NewKnowledgeReindexer(db protocol.SQLQuerier, embedder ChunkEmbedder, index VectorIndexWriter) *KnowledgeReindexer {
	return &KnowledgeReindexer{
		db:        db,
		embedder:  embedder,
		index:     index,
		batchSize: knowledgeReindexBatchSize,
		kick:      make(chan struct{}, 1),
	}
}

// WithGate 注入每轮资源准入（启动期组合根调用）。
func (r *KnowledgeReindexer) WithGate(g func() (release func(), ok bool)) *KnowledgeReindexer {
	r.gate = g
	return r
}

// WithResultHook 注入每轮结果回调（启动期组合根调用，用于嵌入退避器）。
func (r *KnowledgeReindexer) WithResultHook(h func(err error)) *KnowledgeReindexer {
	r.onResult = h
	return r
}

// Kick 非阻塞唤醒后台循环；已有待处理唤醒时合并。nil 接收者安全。
func (r *KnowledgeReindexer) Kick() {
	if r == nil {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

type pendingChunk struct {
	rowid   int64
	id      string
	content string
}

// listPending 读取 rowid > afterRowID 的一批待索引块（按 rowid 升序），读完并关闭 Rows 后才返回
// （R1.16：嵌入期间不得持有读游标）。ADR-0113 决策七：游标让本轮内失败行不再阻塞后续批次。
func (r *KnowledgeReindexer) listPending(ctx context.Context, version string, afterRowID int64) ([]pendingChunk, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT rowid, id, content FROM rag_chunks
		 WHERE deleted_at IS NULL AND embed_model_version != ? AND rowid > ?
		 ORDER BY rowid LIMIT ?`, version, afterRowID, r.batchSize)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "knowledge_reindexer: query pending failed", err)
	}
	defer rows.Close()

	var batch []pendingChunk
	for rows.Next() {
		var c pendingChunk
		if scanErr := rows.Scan(&c.rowid, &c.id, &c.content); scanErr != nil {
			slog.Warn("knowledge_reindexer: row scan failed, row skipped", "err", scanErr)
			continue
		}
		batch = append(batch, c)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "knowledge_reindexer: rows iteration failed", rowsErr)
	}
	if closeErr := rows.Close(); closeErr != nil {
		slog.Warn("knowledge_reindexer: rows close error", "err", closeErr)
	}
	return batch, nil
}

// RunOnce 从头处理一批待索引块，返回成功处理数。引擎未就绪（ModelVersion()==""）时直接返回 0。
// 连续 knowledgeReindexMaxConsecutiveEmbedFails 条嵌入失败视为后端不可用，中止本批并返回错误。
func (r *KnowledgeReindexer) RunOnce(ctx context.Context) (int, error) {
	version := r.embedder.ModelVersion()
	if version == "" {
		return 0, nil
	}
	res, err := r.runBatch(ctx, version, 0)
	return res.processed, err
}

// batchResult 单批结果。scanned==0 表示游标之后已无待索引行；versionChanged 表示批内模型被热切换。
type batchResult struct {
	processed      int
	scanned        int
	lastRowID      int64
	versionChanged bool
}

// runBatch 处理 rowid > afterRowID 的一批。lastRowID 为本批取到的最大 rowid（含失败行），供 drain 推进游标。
func (r *KnowledgeReindexer) runBatch(ctx context.Context, version string, afterRowID int64) (batchResult, error) {
	res := batchResult{lastRowID: afterRowID}
	batch, err := r.listPending(ctx, version, afterRowID)
	if err != nil {
		metrics.RecordKnowledgeReadFailure(ctx, "reindex_list_pending")
		return res, err
	}
	res.scanned = len(batch)
	if len(batch) > 0 {
		res.lastRowID = batch[len(batch)-1].rowid
	}

	consecutiveFails := 0
	for _, c := range batch {
		if ctx.Err() != nil {
			return res, ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份
		}
		vec, embedErr := r.embedder.Embed(ctx, c.content)
		if embedErr == nil && len(vec) == 0 {
			embedErr = apperr.New(apperr.CodeInternal, "knowledge_reindexer: embedder returned empty vector")
		}
		if embedErr != nil {
			consecutiveFails++
			metrics.RecordKnowledgeReadFailure(ctx, "reindex_embed")
			slog.Warn("knowledge_reindexer: embed failed, chunk skipped",
				"id", c.id, "err", embedErr, "consecutive", consecutiveFails)
			if consecutiveFails >= knowledgeReindexMaxConsecutiveEmbedFails {
				return res, apperr.Wrap(apperr.CodeProviderExhausted,
					"knowledge_reindexer: embedder failing consecutively, batch aborted", embedErr)
			}
			continue
		}
		consecutiveFails = 0

		// 嵌入期间模型被热切换：这条向量属于新版本，不能盖旧版本戳，留待下一轮按新版本重嵌。
		if cur := r.embedder.ModelVersion(); cur != version {
			slog.Info("knowledge_reindexer: model version changed mid-batch, aborting batch",
				"from", version, "to", cur)
			res.versionChanged = true
			return res, nil
		}
		if upErr := r.index.VecUpsert(c.id, vec); upErr != nil {
			metrics.RecordKnowledgeReadFailure(ctx, "reindex_vec_upsert")
			slog.Warn("knowledge_reindexer: vec_upsert failed, version kept", "id", c.id, "err", upErr)
			continue
		}
		if _, updErr := r.db.ExecContext(ctx,
			`UPDATE rag_chunks SET embed_model_version = ? WHERE id = ?`, version, c.id); updErr != nil {
			metrics.RecordKnowledgeReadFailure(ctx, "reindex_update_version")
			slog.Warn("knowledge_reindexer: version write-back failed", "id", c.id, "err", updErr)
			continue
		}
		res.processed++
		runtime.Gosched()
	}
	if res.processed > 0 {
		slog.Info("knowledge_reindexer: batch done", "processed", res.processed, "pending_in_batch", len(batch), "version", version)
	}
	return res, nil
}

// drain 单轮内按 rowid 游标连续推进批次：取到 0 行即结束；本批全部失败也继续推进下一批
// （ADR-0113 决策七，防毒行饿死）。中止错误（连续嵌入失败 / ctx）直接返回；版本在轮首取一次，轮内变化则结束本轮。
func (r *KnowledgeReindexer) drain(ctx context.Context) error {
	version := r.embedder.ModelVersion()
	if version == "" {
		return nil
	}
	var cursor int64
	for i := 0; i < knowledgeReindexMaxBatchesPerRound; i++ {
		res, err := r.runBatch(ctx, version, cursor)
		if err != nil {
			return err
		}
		if res.scanned == 0 || res.versionChanged {
			return nil
		}
		cursor = res.lastRowID
		if r.embedder.ModelVersion() != version {
			return nil
		}
	}
	return nil
}

func (r *KnowledgeReindexer) round(ctx context.Context) {
	if r.gate != nil {
		release, ok := r.gate()
		if !ok {
			return
		}
		defer release()
	}
	err := r.drain(ctx)
	if err != nil && ctx.Err() == nil {
		slog.Warn("knowledge_reindexer: round failed", "err", err)
	}
	if r.onResult != nil && ctx.Err() == nil {
		r.onResult(err)
	}
}

// Loop 阻塞运行后台循环：定时 tick（interval）+ Kick 唤醒；ctx 取消即退出（ADR-0087 决策五）。
// 调用方用 concurrent.SafeGo 启动。启动时先跑一轮，收敛上次进程遗留的积压。
func (r *KnowledgeReindexer) Loop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = KnowledgeReindexInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.round(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.round(ctx)
		case <-r.kick:
			r.round(ctx)
		}
	}
}
