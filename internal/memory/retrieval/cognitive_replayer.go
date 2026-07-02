package retrieval

import (
	"context"
	"fmt"
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

		rows, err := cr.db.QueryContext(ctx, "SELECT id, content, embedding FROM episodic_events WHERE archived = 0 ORDER BY id ASC LIMIT ? OFFSET ?", cr.batchSize, offset)
		if err != nil {
			return err
		}

		var count int
		for rows.Next() {
			var id int
			var content string
			var embBlob []byte
			if err := rows.Scan(&id, &content, &embBlob); err != nil {
				rows.Close()
				return err
			}
			
			// According to schema, IDs are integers. We need to prefix them.
			eventID := fmt.Sprintf("evt_%d", id) 
			
			// FTSIndex
			if err := cr.cognitive.FTSIndex(eventID, content); err != nil {
				slog.WarnContext(ctx, "cognitive replayer: failed to index episodic FTS", "id", eventID, "err", err)
			}
			
			// Vector
			if len(embBlob) > 0 {
				vec := DecodeFloat16(embBlob)
				if err := cr.cognitive.VecUpsert(eventID, vec); err != nil {
					slog.WarnContext(ctx, "cognitive replayer: failed to index episodic Vector", "id", eventID, "err", err)
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

		rows, err := cr.db.QueryContext(ctx, "SELECT entity_type, name, description FROM semantic_entities WHERE status = 'active' ORDER BY entity_type, name LIMIT ? OFFSET ?", cr.batchSize, offset)
		if err != nil {
			return err
		}

		var count int
		for rows.Next() {
			var entityType, name, description string
			if err := rows.Scan(&entityType, &name, &description); err != nil {
				rows.Close()
				return err
			}
			
			docID := "sement_" + entityType + "_" + name
			text := name + " " + description
			
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

		rows, err := cr.db.QueryContext(ctx, "SELECT source, target, relation_type, weight FROM semantic_edges ORDER BY source, target LIMIT ? OFFSET ?", cr.batchSize, offset)
		if err != nil {
			// table might not exist in some tests
			slog.DebugContext(ctx, "cognitive replayer: failed to query semantic_edges, skipping", "err", err)
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
