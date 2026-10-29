package retrieval

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// insertVecEvent 插入带向量、指定版本的情景事件（ADR-0113 决策四测试辅助）。
func insertVecEvent(t *testing.T, db *sql.DB, content, version string, vec []float32) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO episodic_events (content, embedding, embed_model_version, event_uuid) VALUES (?, ?, ?, ?)`,
		content, encodeFloat16(vec), version, "uuid-"+content,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func fragContents(frags []string) string { return strings.Join(frags, ",") }

func TestFetchVectorResultsFromSQL_VersionFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	q := []float32{1, 0, 0, 0}
	insertVecEvent(t, db, "old", "v1", q)
	insertVecEvent(t, db, "cur", "v2", q)

	run := func(hr *HybridRetrieverImpl) []string {
		frags := hr.fetchVectorResultsFromSQL(context.Background(), db, q, 10)
		got := make([]string, 0, len(frags))
		for _, f := range frags {
			got = append(got, f.Content)
		}
		return got
	}

	t.Run("current version only", func(t *testing.T) {
		hr := &HybridRetrieverImpl{embedder: &stubEmbedder{version: "v2", dim: 4}}
		got := run(hr)
		if len(got) != 1 || got[0] != "cur" {
			t.Fatalf("want only [cur], got [%s]", fragContents(got))
		}
	})

	t.Run("embedder nil skips vector path", func(t *testing.T) {
		hr := &HybridRetrieverImpl{}
		if got := run(hr); len(got) != 0 {
			t.Fatalf("want empty, got [%s]", fragContents(got))
		}
	})

	t.Run("empty version skips vector path", func(t *testing.T) {
		hr := &HybridRetrieverImpl{embedder: &stubEmbedder{version: "", dim: 4}}
		if got := run(hr); len(got) != 0 {
			t.Fatalf("want empty, got [%s]", fragContents(got))
		}
	})

	t.Run("no row for version", func(t *testing.T) {
		hr := &HybridRetrieverImpl{embedder: &stubEmbedder{version: "v3", dim: 4}}
		if got := run(hr); len(got) != 0 {
			t.Fatalf("want empty, got [%s]", fragContents(got))
		}
	})
}

func TestFetchVectorResultsFromSQL_QueryFailureDegradesEmpty(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`DROP TABLE episodic_events`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	defer db.Close()
	hr := &HybridRetrieverImpl{embedder: &stubEmbedder{version: "v2", dim: 4}}
	if got := hr.fetchVectorResultsFromSQL(context.Background(), db, []float32{1, 0, 0, 0}, 10); len(got) != 0 {
		t.Fatalf("want empty on query failure, got %d", len(got))
	}
}
