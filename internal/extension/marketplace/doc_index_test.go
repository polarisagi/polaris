package marketplace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockDocIndexRepo struct {
	protocol.ExtensionRepository
	rows []types.ExtDocIndexRow
	meta map[string]string
}

func (m *mockDocIndexRepo) ReplaceDocIndex(ctx context.Context, rows []types.ExtDocIndexRow, meta map[string]string) error {
	m.rows = rows
	m.meta = meta
	return nil
}

func (m *mockDocIndexRepo) GetDocIndexEntry(ctx context.Context, catalogID string) (*types.ExtDocIndexRow, error) {
	for _, r := range m.rows {
		if r.CatalogID == catalogID {
			return &r, nil
		}
	}
	return nil, nil
}

func (m *mockDocIndexRepo) GetDocIndexMeta(ctx context.Context, key string) (string, error) {
	if m.meta != nil {
		return m.meta[key], nil
	}
	return "", nil
}

func TestSanitizeDocIndex(t *testing.T) {
	catID := "polarisagi/polaris-plugins-official/weather-pro"
	expectedKey := DocumentKey(catID)
	hashEn := strings.Repeat("a", 64)
	hashZh := strings.Repeat("b", 64)

	idx := &DocIndex{
		Schema:      1,
		GeneratedAt: "2026-11-15T02:00:00Z",
		Entries: map[string]DocIndexEntry{
			catID: {
				MarketplaceID: "polarisagi/polaris-plugins-official",
				DocumentKey:   expectedKey,
				Source: DocSource{
					RepoURL:  "https://github.com/polarisagi/polaris-plugins-official.git",
					Revision: "3dc809ae",
					Path:     "plugins/weather-pro/README.md",
					Kind:     "readme",
					SHA256:   hashEn,
					BaseURL:  "https://raw.githubusercontent.com/polarisagi/...",
				},
				Locales: map[string]DocLocale{
					"zh-CN": {
						Path:             "zh-CN/weather-pro/README_zh.md",
						SHA256:           hashZh,
						Origin:           "upstream",
						Review:           "reviewed",
						SourceSHA256:     hashEn,
						GeneratorVersion: "v1.0.0",
					},
				},
			},
		},
	}

	rows, err := SanitizeDocIndex(idx)
	if err != nil {
		t.Fatalf("SanitizeDocIndex failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 sanitized row, got %d", len(rows))
	}
	if rows[0].CatalogID != catID || rows[0].DocumentKey != expectedKey {
		t.Fatalf("row mismatch: %+v", rows[0])
	}

	// 测试非法条目拒收（A9）
	badIdx := &DocIndex{
		Schema: 1,
		Entries: map[string]DocIndexEntry{
			"bad/key": {
				DocumentKey: "wrong_key",
			},
		},
	}
	if _, err := SanitizeDocIndex(badIdx); err == nil {
		t.Fatal("expected error for bad doc index entry, got nil")
	}
}

func TestDocIndexSync_Fetch(t *testing.T) {
	catID := "test-org/tool"
	key := DocumentKey(catID)
	hashEn := strings.Repeat("c", 64)
	hashZh := strings.Repeat("d", 64)
	rawJSON := `{
		"schema": 1,
		"generated_at": "2026-11-15T02:00:00Z",
		"entries": {
			"` + catID + `": {
				"marketplace_id": "test-org",
				"document_key": "` + key + `",
				"source": {
					"repo_url": "https://example.com/repo.git",
					"revision": "c1",
					"path": "README.md",
					"sha256": "` + hashEn + `"
				},
				"locales": {
					"zh-CN": {
						"path": "zh-CN/tool/README_zh.md",
						"sha256": "` + hashZh + `",
						"origin": "upstream",
						"review": "reviewed",
						"source_sha256": "` + hashEn + `"
					}
				}
			}
		}
	}`

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"etag-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"etag-1"`)
		w.Write([]byte(rawJSON))
	}))
	defer ts.Close()

	repo := &mockDocIndexRepo{}
	syncer := NewDocIndexSync(repo, network.SafeHTTPClient{Client: ts.Client()})

	ctx := context.Background()
	n, err := syncer.Sync(ctx, ts.URL)
	if err != nil {
		t.Fatalf("syncer.Sync failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 synced, got %d", n)
	}

	// 再次调用，ETag 匹配返回 304，n=0
	n2, err := syncer.Sync(ctx, ts.URL)
	if err != nil {
		t.Fatalf("second Sync failed: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("expected 0 for 304, got %d", n2)
	}
}
