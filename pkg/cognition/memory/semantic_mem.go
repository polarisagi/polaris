package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
)

// IntentSubmitter abstracts the MutationBus submission to avoid circular dependency.
type IntentSubmitter interface {
	Submit(ctx context.Context, intent *pb.MutationIntent) error
}

// DBAccessor allows fetching the underlying sql.DB from the store
type DBAccessor interface {
	DB() *sql.DB
}

// ============================================================================
// SemanticMemory (L2) — 文档/实体存储
// ============================================================================

type SemanticMem struct {
	store protocol.Store
	bus   IntentSubmitter
}

func NewSemanticMem(store protocol.Store, bus IntentSubmitter) *SemanticMem {
	return &SemanticMem{store: store, bus: bus}
}

func (sm *SemanticMem) StoreDocument(ctx context.Context, doc protocol.Document) error {
	key := []byte("doc:" + doc.ID)
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return sm.store.Put(ctx, key, data)
}

func (sm *SemanticMem) StoreChunks(ctx context.Context, docID string, chunks []protocol.Chunk) error {
	for _, ch := range chunks {
		key := []byte("chunk:" + ch.ID)
		data, err := json.Marshal(ch)
		if err != nil {
			return err
		}
		if err := sm.store.Put(ctx, key, data); err != nil {
			return err
		}
	}
	return nil
}

func (sm *SemanticMem) GetDocument(ctx context.Context, id string) (*protocol.Document, error) {
	data, err := sm.store.Get(ctx, []byte("doc:"+id))
	if err != nil {
		return nil, err
	}
	var doc protocol.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func (sm *SemanticMem) Archive(ctx context.Context, id string, reason string) error {
	doc, err := sm.GetDocument(ctx, id)
	if err != nil {
		return err
	}
	doc.Archived = true
	return sm.StoreDocument(ctx, *doc)
}

func (sm *SemanticMem) UpsertFact(ctx context.Context, entity protocol.Entity) error {
	payload, err := json.Marshal(entity)
	if err != nil {
		return err
	}
	intent := &pb.MutationIntent{
		Table:     "semantic_entities",
		Operation: "upsert",
		Payload:   payload,
	}
	if sm.bus != nil {
		return sm.bus.Submit(ctx, intent)
	}
	return perrors.New(perrors.CodeInternal, "MutationBus not configured in SemanticMem")
}

func (sm *SemanticMem) UpsertRelation(ctx context.Context, rel protocol.Relation) error {
	payload, err := json.Marshal(rel)
	if err != nil {
		return err
	}
	intent := &pb.MutationIntent{
		Table:     "semantic_relations",
		Operation: "upsert",
		Payload:   payload,
	}
	if sm.bus != nil {
		return sm.bus.Submit(ctx, intent)
	}
	return perrors.New(perrors.CodeInternal, "MutationBus not configured in SemanticMem")
}

func (sm *SemanticMem) GetEntity(ctx context.Context, entityType, name string) (*protocol.Entity, error) {
	dbAccess, ok := sm.store.(DBAccessor)
	if !ok {
		return nil, perrors.New(perrors.CodeInternal, "Store does not implement DBAccessor")
	}
	db := dbAccess.DB()
	if db == nil {
		return nil, perrors.New(perrors.CodeInternal, "Underlying DB is nil")
	}

	row := db.QueryRowContext(ctx, "SELECT id, name, entity_type, properties, embedding, source_event_id, version FROM semantic_entities WHERE entity_type = ? AND name = ?", entityType, name)

	var ent protocol.Entity
	var propertiesJSON []byte
	var embeddingBytes []byte
	var idInt int64

	err := row.Scan(&idInt, &ent.Name, &ent.Type, &propertiesJSON, &embeddingBytes, &ent.SourceEventID, &ent.Version)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, perrors.New(perrors.CodeNotFound, "Entity not found")
		}
		return nil, err
	}

	ent.ID = "entity:" + strconv.FormatInt(idInt, 10)

	if len(propertiesJSON) > 0 {
		if err := json.Unmarshal(propertiesJSON, &ent.Properties); err != nil {
			return nil, err
		}
	}
	// For embeddingBytes, in a real implementation we would convert float16 byte slice to []float32.
	// Since we don't have the explicit quantizer here, we skip it or mock it.

	return &ent, nil
}

// ============================================================================
// ProceduralMemory (L3) — 委托 M6 SkillRegistry
// ============================================================================

type ProceduralMem struct {
	skills protocol.SkillRegistry
}

func (pm *ProceduralMem) Skills() protocol.SkillRegistry {
	return pm.skills
}
