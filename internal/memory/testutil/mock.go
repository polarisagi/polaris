package testutil

import (
	"errors"

	"context"
	"database/sql"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/types"
)

// MockStore implements protocol.Store for testing
type MockStore struct {
	data map[string][]byte
	db   *sql.DB
}

func NewMockStore() *MockStore {
	db, _ := sql.Open("sqlite3", ":memory:")

	// Create required tables — DDL 与 internal/protocol/schema/004_semantic_memory.sql 保持一致
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS semantic_entities (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			entity_type     TEXT    NOT NULL DEFAULT '',
			name            TEXT    NOT NULL DEFAULT '',
			status          TEXT    NOT NULL DEFAULT 'active',
			superseded_by   INTEGER,
			properties      TEXT,
			embedding       BLOB,
			version         INTEGER NOT NULL DEFAULT 1,
			created_at      INTEGER NOT NULL DEFAULT 0,
			updated_at      INTEGER NOT NULL DEFAULT 0,
			source_event_id INTEGER,
			confidence      REAL    NOT NULL DEFAULT 1.0,
			source_type     TEXT    NOT NULL DEFAULT 'llm_extract',
			valid_from      INTEGER,
			valid_until     INTEGER,
			taint_level     INTEGER NOT NULL DEFAULT 0,
			UNIQUE(entity_type, name)
		);
		CREATE TABLE IF NOT EXISTS semantic_relations (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id       INTEGER REFERENCES semantic_entities(id),
			target_id       INTEGER REFERENCES semantic_entities(id),
			relation_type   TEXT NOT NULL,
			weight          REAL DEFAULT 1.0,
			properties      TEXT,
			created_at      INTEGER NOT NULL DEFAULT 0,
			source_event_id INTEGER,
			updated_at      INTEGER NOT NULL DEFAULT 0,
			confidence      REAL NOT NULL DEFAULT 1.0,
			taint_level     INTEGER NOT NULL DEFAULT 0,
			UNIQUE(source_id, target_id, relation_type)
		);
		CREATE TABLE IF NOT EXISTS episodic_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL DEFAULT '',
			seq INTEGER NOT NULL DEFAULT 0,
			timestamp INTEGER NOT NULL DEFAULT 0,
			event_type TEXT NOT NULL DEFAULT 'intent',
			source TEXT NOT NULL DEFAULT 'agent',
			content TEXT NOT NULL DEFAULT '',
			embedding BLOB,
			salience REAL NOT NULL DEFAULT 0.5,
			decay_weight REAL NOT NULL DEFAULT 1.0,
			occurred_at INTEGER,
			embed_model_version TEXT NOT NULL DEFAULT '',
			event_uuid TEXT NOT NULL DEFAULT '',
			archived BOOLEAN NOT NULL DEFAULT 0,
			archive_offset INTEGER,
			reasoning_state TEXT
		);
		CREATE TABLE IF NOT EXISTS user_profile (
			profile_key TEXT PRIMARY KEY,
			stable_facts TEXT,
			recent_activity TEXT,
			behavioral_patterns TEXT,
			synthesis_count INTEGER,
			last_event_ts INTEGER,
			created_at INTEGER,
			updated_at INTEGER
		);
	`)

	return &MockStore{
		data: make(map[string][]byte),
		db:   db,
	}
}

func (m *MockStore) DB() *sql.DB {
	return m.db
}

func (m *MockStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	if v, ok := m.data[string(key)]; ok {
		return v, nil
	}
	return nil, nil // Assume no error, just not found
}

func (m *MockStore) Put(ctx context.Context, key, value []byte) error {
	m.data[string(key)] = value
	return nil
}

func (m *MockStore) Delete(ctx context.Context, key []byte) error {
	delete(m.data, string(key))
	return nil
}

type MockIterator struct {
	keys   [][]byte
	values [][]byte
	idx    int
}

func (it *MockIterator) Next() bool {
	it.idx++
	return it.idx < len(it.keys)
}
func (it *MockIterator) Key() []byte   { return it.keys[it.idx] }
func (it *MockIterator) Value() []byte { return it.values[it.idx] }
func (it *MockIterator) Close() error  { return nil }
func (it *MockIterator) Err() error    { return nil }

func (m *MockStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	var keys [][]byte
	var values [][]byte
	for k, v := range m.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == string(prefix) {
			keys = append(keys, []byte(k))
			values = append(values, v)
		}
	}
	return &MockIterator{keys: keys, values: values, idx: -1}, nil
}

func (m *MockStore) BatchWrite(ctx context.Context, ops []types.Op) error {
	for _, op := range ops {
		switch op.Type {
		case types.OpPut:
			m.data[string(op.Key)] = op.Value
		case types.OpDelete:
			delete(m.data, string(op.Key))
		}
	}
	return nil
}

type MockTx struct {
	m *MockStore
}

func (tx *MockTx) Get(key []byte) ([]byte, error) { return tx.m.Get(context.Background(), key) }
func (tx *MockTx) Put(key, value []byte) error    { return tx.m.Put(context.Background(), key, value) }
func (tx *MockTx) Delete(key []byte) error        { return tx.m.Delete(context.Background(), key) }
func (tx *MockTx) Scan(prefix []byte) (protocol.Iterator, error) {
	return tx.m.Scan(context.Background(), prefix)
}
func (tx *MockTx) Commit(ctx context.Context) error   { return nil }
func (tx *MockTx) Rollback(ctx context.Context) error { return nil }

func (m *MockStore) Txn(ctx context.Context, fn func(tx protocol.Transaction) error) error {
	return fn(&MockTx{m: m})
}

func (m *MockStore) Capabilities() types.StoreCapabilities {
	return types.StoreCapabilities{}
}

func (m *MockStore) Close() error {
	return nil
}

type MockIntentSubmitter struct{}

func (m *MockIntentSubmitter) Submit(ctx context.Context, intent *pb.MutationIntent) error {
	return nil
}

// Ensure MockStore implements protocol.Store
var _ protocol.Store = (*MockStore)(nil)

// MockGraphTraverser implements GraphTraverser
type MockGraphTraverser struct{}

func (m *MockGraphTraverser) GraphTraverse(startID, edgeType string, maxDepth int) ([]string, error) {
	return nil, nil
}
func (m *MockGraphTraverser) GraphRelate(fromID, edgeType, toID string, weight float64) error {
	return nil
}
func (m *MockGraphTraverser) SpreadingActivation(_ []string, _ int, _, _ float64, _ int) ([]types.ScoredNode, error) {
	return nil, nil
}

var _ protocol.GraphTraverser = (*MockGraphTraverser)(nil)

var ErrProviderFailed = errors.New("provider failed")

type MockProvider struct {
	Fail bool
	Resp string
}

func (m *MockProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	if m.Fail {
		return nil, ErrProviderFailed
	}
	return &types.ProviderResponse{
		Content: m.Resp,
	}, nil
}
func (m *MockProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (m *MockProvider) SetOptions(opts ...types.InferOption) {}
func (m *MockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *MockProvider) MaxContextLength() int                    { return 1000 }
func (m *MockProvider) CountTokens(text string) int              { return 0 }
func (m *MockProvider) CountMessageTokens(msg types.Message) int { return 0 }
func (m *MockProvider) Name() string                             { return "mock" }
func (m *MockProvider) Ping(ctx context.Context) error           { return nil }

func (m *MockProvider) ModelID() string                      { return "mock" }
func (m *MockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
