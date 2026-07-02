package retrieval

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

type CognitiveReplayer struct {
	db        protocol.SQLQuerier
	cognitive protocol.CognitiveSearcher
	batchSize int
}

func NewCognitiveReplayer(db protocol.SQLQuerier, cognitive protocol.CognitiveSearcher) *CognitiveReplayer {
	return &CognitiveReplayer{
		db:        db,
		cognitive: cognitive,
		batchSize: 50,
	}
}

func (cr *CognitiveReplayer) Start(ctx context.Context) error {
	go cr.replay(ctx)
	return nil
}

func (cr *CognitiveReplayer) replay(ctx context.Context) {
	slog.InfoContext(ctx, "cognitive replayer: starting background replay")
	start := time.Now()

	// 1. Replay episodic_events
	if err := cr.replayEpisodic(ctx); err != nil {
		slog.ErrorContext(ctx, "cognitive replayer: failed to replay episodic_events", "err", err)
	}

	// 2. Replay semantic_entities
	if err := cr.replaySemantic(ctx); err != nil {
		slog.ErrorContext(ctx, "cognitive replayer: failed to replay semantic_entities", "err", err)
	}

	slog.InfoContext(ctx, "cognitive replayer: background replay completed", "duration", time.Since(start))
}

func (cr *CognitiveReplayer) replayEpisodic(ctx context.Context) error {
	var offset int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// event_uuid 是原始 Event.ID（UUID）——热路径写入（EpisodicMem.Append → FTSIndex(ev.ID)）
		// 与检索回读（"episodic:"+hit.ID 取 KV 原文）均以 UUID 为键，重放必须使用同一 ID 空间。
		// event_uuid 为空的行（旧数据/未投影完成）无法关联，跳过。
		rows, err := cr.db.QueryContext(ctx,
			"SELECT event_uuid, content, embedding FROM episodic_events WHERE archived = 0 AND event_uuid != '' ORDER BY id ASC LIMIT ? OFFSET ?",
			cr.batchSize, offset)
		if err != nil {
			return err
		}

		var count int
		for rows.Next() {
			var eventID, content string
			var embBlob []byte
			if err := rows.Scan(&eventID, &content, &embBlob); err != nil {
				rows.Close()
				return err
			}

			// FTSIndex
			if err := cr.cognitive.FTSIndex(eventID, content); err != nil {
				slog.WarnContext(ctx, "cognitive replayer: failed to index episodic FTS", "id", eventID, "err", err)
			}

			// Vector（float16 BLOB 解码失败返回 nil，跳过防止写入空向量）
			if len(embBlob) > 0 {
				if vec := DecodeFloat16(embBlob); vec != nil {
					if err := cr.cognitive.VecUpsert(eventID, vec); err != nil {
						slog.WarnContext(ctx, "cognitive replayer: failed to index episodic Vector", "id", eventID, "err", err)
					}
				}
			}
			count++
		}
		rows.Close()

		if count < cr.batchSize {
			break
		}
		offset += cr.batchSize
		runtime.Gosched()
	}
	return nil
}

func (cr *CognitiveReplayer) replaySemantic(ctx context.Context) error {
	var offset int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// semantic_entities 无 description 列（schema/004）：描述性内容在 properties JSON 中，
		// 与 SemanticMem.UpsertFact 写时双写的 FTS 文本口径保持一致（name + properties）。
		rows, err := cr.db.QueryContext(ctx,
			"SELECT entity_type, name, COALESCE(properties, '') FROM semantic_entities WHERE status = 'active' ORDER BY entity_type, name LIMIT ? OFFSET ?",
			cr.batchSize, offset)
		if err != nil {
			return err
		}

		var count int
		for rows.Next() {
			var entityType, name, propsJSON string
			if err := rows.Scan(&entityType, &name, &propsJSON); err != nil {
				rows.Close()
				return err
			}

			docID := "sement_" + entityType + "_" + name
			text := name + " " + propsJSON

			// FTSIndex
			if err := cr.cognitive.FTSIndex(docID, text); err != nil {
				slog.WarnContext(ctx, "cognitive replayer: failed to index semantic FTS", "id", docID, "err", err)
			}
			count++
		}
		rows.Close()

		if count < cr.batchSize {
			break
		}
		offset += cr.batchSize
		runtime.Gosched()
	}

	// Semantic edges graph replay
	offset = 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// 关系表权威名为 semantic_relations（schema/004），source_id/target_id 是实体主键，
		// 需 JOIN 回实体名——SurrealDB 图边以实体名为节点 ID（与 EpisodicGraphIndexer 口径一致）。
		rows, err := cr.db.QueryContext(ctx, `
			SELECT se_from.name, se_to.name, sr.relation_type, sr.weight
			FROM semantic_relations sr
			JOIN semantic_entities se_from ON se_from.id = sr.source_id
			JOIN semantic_entities se_to   ON se_to.id = sr.target_id
			ORDER BY sr.id LIMIT ? OFFSET ?`, cr.batchSize, offset)
		if err != nil {
			slog.WarnContext(ctx, "cognitive replayer: failed to query semantic_relations, skipping graph replay", "err", err)
			break
		}

		var count int
		for rows.Next() {
			var from, to, rel string
			var weight float64
			if err := rows.Scan(&from, &to, &rel, &weight); err != nil {
				rows.Close()
				return err
			}

			if err := cr.cognitive.GraphRelate(from, rel, to, weight); err != nil {
				slog.WarnContext(ctx, "cognitive replayer: failed to index graph edge", "from", from, "to", to, "err", err)
			}
			count++
		}
		rows.Close()

		if count < cr.batchSize {
			break
		}
		offset += cr.batchSize
		runtime.Gosched()
	}

	return nil
}
