package graphrag

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// chunkTaintModuleForTest 必须与 internal/knowledge/taint_boundary.go 内部
// 常量 chunkTaintModule 的值一致——两者是同一份跨模块 HMAC 签名口径的独立副本
// （graphrag 不得反向依赖 knowledge 根包，见 ChunkTaintSealer 接口注释），
// 修改任一处时须同步。
const chunkTaintModuleForTest = "knowledge.rag_chunks"

// testChunkSealer 是测试专用的 ChunkTaintSealer 实现，直接调用 taint 包
// （不经 knowledge.ChunkTaintSealerAdapter，避免 graphrag test → knowledge →
// graphrag 的包循环）。
type testChunkSealer struct {
	ser *taint.TaintBoundarySerializer
}

func (s *testChunkSealer) SealChunkTaint(id, content string, level int, source string) string {
	ts := taint.NewTaintedString(content, taint.TaintSource{
		Module:           chunkTaintModuleForTest,
		EntityID:         id,
		OriginTaintLevel: types.TaintLevel(level),
	}, source)
	return s.ser.Seal(ts).HMACHex
}

// verifyTestChunkTaint 复刻 internal/knowledge/taint_boundary.go 的
// verifyChunkTaint 校验逻辑，用于独立验证 summary_gen_handler.go 写入的
// taint_hmac 能否被正确校验（而非走 fail-closed 兜底）。
func verifyTestChunkTaint(ser *taint.TaintBoundarySerializer, id, content string, level int, source, hmacHex string) types.TaintLevel {
	if hmacHex == "" {
		return types.TaintHigh
	}
	env := taint.TaintEnvelope{
		Content: content,
		Level:   types.TaintLevel(level),
		Source: taint.TaintSource{
			Module:           chunkTaintModuleForTest,
			EntityID:         id,
			OriginTaintLevel: types.TaintLevel(level),
		},
		HMACHex: hmacHex,
	}
	recovered, ok := ser.Unseal(env)
	if !ok {
		return types.TaintHigh
	}
	return recovered.Level()
}

func setupSummaryGenTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "polaris_summary_gen_db")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "summary_gen.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`CREATE TABLE rag_chunks (
		id            TEXT PRIMARY KEY,
		doc_id        TEXT NOT NULL,
		content       TEXT NOT NULL,
		taint_level   INTEGER NOT NULL DEFAULT 1,
		taint_source  TEXT,
		taint_hmac    TEXT NOT NULL DEFAULT '',
		chunk_type    TEXT NOT NULL DEFAULT 'leaf',
		chunk_index   INTEGER NOT NULL DEFAULT 0,
		deleted_at    TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}

	return db, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// TestSummaryGenHandler_InheritsSourceTaintAndSigns_S05 验证 S-05 修复：源
// chunk 为 TaintHigh 时，生成的摘要行 taint_level 必须继承为 TaintHigh（而非
// 硬编码 0），且 taint_hmac 非空、能通过校验恢复出 TaintHigh（而非因签名缺失
// 被 fail-closed 兜底判定为 TaintHigh——这两种情况从最终数值上看似相同，
// 但本测试通过 hmacHex != "" 断言排除了"巧合命中 fail-closed 默认值"的假阳性）。
func TestSummaryGenHandler_InheritsSourceTaintAndSigns_S05(t *testing.T) {
	db, cleanup := setupSummaryGenTestDB(t)
	defer cleanup()

	docID := "doc-1"
	_, err := db.Exec(
		`INSERT INTO rag_chunks (id, doc_id, content, taint_level, chunk_type, chunk_index) VALUES (?,?,?,?,?,?)`,
		"chunk-1", docID, "source paragraph one", int(types.TaintHigh), "parent", 0)
	if err != nil {
		t.Fatalf("seed chunk failed: %v", err)
	}

	ser := taint.NewTaintBoundarySerializer([]byte("test-hmac-key-0123456789"))
	sealer := &testChunkSealer{ser: ser}
	provider := &mockProvider{content: "这是一份摘要。"}
	handler := NewSummaryGenOutboxHandler(db, provider, sealer)

	if err := handler.generateSummary(context.Background(), docID); err != nil {
		t.Fatalf("generateSummary failed: %v", err)
	}

	summaryID := "doc_summary_" + docID
	var taintLevel int
	var taintHMAC, content string
	row := db.QueryRow(`SELECT taint_level, taint_hmac, content FROM rag_chunks WHERE id = ?`, summaryID)
	if err := row.Scan(&taintLevel, &taintHMAC, &content); err != nil {
		t.Fatalf("query summary row failed: %v", err)
	}

	if types.TaintLevel(taintLevel) != types.TaintHigh {
		t.Errorf("expected summary taint_level == TaintHigh, got %v", types.TaintLevel(taintLevel))
	}
	if taintHMAC == "" {
		t.Fatal("expected non-empty taint_hmac, got empty string (签名链断裂)")
	}

	recovered := verifyTestChunkTaint(ser, summaryID, content, taintLevel, "outbox_summary", taintHMAC)
	if recovered != types.TaintHigh {
		t.Errorf("expected verifyChunkTaint to recover TaintHigh via valid signature, got %v", recovered)
	}
}

// TestSummaryGenHandler_SourceTaintReadFailure_FailsClosedToHigh_S05 验证源污点
// 读取失败时摘要行 taint_level 必须 fail-closed 为 TaintHigh，禁止取 0。
//
// 构造手法：SQLite 列存在动态类型（type affinity），taint_level 列虽声明为
// INTEGER，但写入非数字 TEXT 时仍会原样保留；该 doc_id 下唯一一行的
// taint_level 是非数字字符串，MAX(taint_level) 返回该字符串，Scan 进 Go int
// 变量必然报错——精确复现"源污点读取失败"分支，而不依赖关闭整个 DB 连接
// （关闭连接会让更早的 parent chunk 查询一并失败，无法定位到目标分支）。
func TestSummaryGenHandler_SourceTaintReadFailure_FailsClosedToHigh_S05(t *testing.T) {
	db, cleanup := setupSummaryGenTestDB(t)
	defer cleanup()

	docID := "doc-2"
	_, err := db.Exec(
		`INSERT INTO rag_chunks (id, doc_id, content, taint_level, chunk_type, chunk_index) VALUES (?,?,?,?,?,?)`,
		"chunk-2", docID, "source paragraph two", "not-a-number", "parent", 0)
	if err != nil {
		t.Fatalf("seed chunk failed: %v", err)
	}

	ser := taint.NewTaintBoundarySerializer([]byte("test-hmac-key-0123456789"))
	sealer := &testChunkSealer{ser: ser}
	provider := &mockProvider{content: "摘要内容"}
	handler := NewSummaryGenOutboxHandler(db, provider, sealer)

	if err := handler.generateSummary(context.Background(), docID); err != nil {
		t.Fatalf("generateSummary should fail-closed and still succeed overall, got error: %v", err)
	}

	summaryID := "doc_summary_" + docID
	var taintLevel int
	if err := db.QueryRow(`SELECT taint_level FROM rag_chunks WHERE id = ?`, summaryID).Scan(&taintLevel); err != nil {
		t.Fatalf("query summary row failed: %v", err)
	}
	if types.TaintLevel(taintLevel) != types.TaintHigh {
		t.Errorf("source taint read failure must fail-closed to TaintHigh, got %v", types.TaintLevel(taintLevel))
	}
}
