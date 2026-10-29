package marketplace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/types"
)

func testOverlayIndex() OverlayIndex {
	return OverlayIndex{
		Schema:      OverlaySchemaVersion,
		GeneratedAt: "2026-10-25T00:00:00Z",
		Taxonomy: OverlayTaxonomy{Version: 1, Values: map[string]map[string]OverlayLabel{
			"topic":      {"code": {En: "Code", Zh: "代码"}, "databases": {En: "Databases", Zh: "数据库"}},
			"department": {"engineering": {En: "Engineering", Zh: "工程"}},
			"industry":   {"education": {En: "Education", Zh: "教育"}},
			"life":       {"music": {En: "Music", Zh: "音乐"}},
		}},
		Entries: map[string]OverlayEntry{
			"mp/a": {H: OverlaySourceHash("a", "desc"), Zh: OverlayZh{Name: "甲", Summary: "摘要", Description: "说明"},
				Topic: []string{"code", "unknown", "code", "databases"}, Department: []string{"engineering"}},
			"":     {H: "x"},                        // 空 ID 丢弃
			"mp/b": {Zh: OverlayZh{Summary: "无指纹"}}, // 无指纹丢弃
		},
	}
}

func TestOverlaySourceHash_StableAndTrimmed(t *testing.T) {
	if OverlaySourceHash("a", "desc") != OverlaySourceHash(" a ", "desc\n") {
		t.Fatal("hash must ignore surrounding whitespace")
	}
	if OverlaySourceHash("a", "desc") == OverlaySourceHash("a", "desc2") {
		t.Fatal("hash must change with description")
	}
	if len(OverlaySourceHash("a", "")) != 12 {
		t.Fatal("hash length must be 12")
	}
}

func TestSanitizeOverlay(t *testing.T) {
	idx := testOverlayIndex()
	long := strings.Repeat("长", overlayMaxDescRunes+50)
	e := idx.Entries["mp/a"]
	e.Zh.Description = long + "\x07"
	idx.Entries["mp/a"] = e

	rows, tax, err := SanitizeOverlay(&idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CatalogID != "mp/a" {
		t.Fatalf("want only mp/a, got %+v", rows)
	}
	if rows[0].Topics != `["code","databases"]` {
		t.Fatalf("unknown/duplicate topics must be dropped, got %s", rows[0].Topics)
	}
	if rows[0].Industries != `[]` {
		t.Fatalf("empty facet must serialize as [], got %s", rows[0].Industries)
	}
	if n := len([]rune(rows[0].DescriptionZh)); n != overlayMaxDescRunes {
		t.Fatalf("description must be truncated to %d runes, got %d", overlayMaxDescRunes, n)
	}
	var parsed OverlayTaxonomy
	if err := json.Unmarshal([]byte(tax), &parsed); err != nil || parsed.Values["topic"]["code"].Zh != "代码" {
		t.Fatalf("taxonomy json invalid: %v %s", err, tax)
	}

	bad := testOverlayIndex()
	bad.Schema = 99
	if _, _, err := SanitizeOverlay(&bad); err == nil {
		t.Fatal("unsupported schema must be rejected")
	}
}

// fakeOverlayRepo 只实现覆盖层三个方法；其余方法经嵌入接口留空（调用即 panic，测试不应触达）。
type fakeOverlayRepo struct {
	protocol.ExtensionRepository
	rows []types.ExtCatalogOverlayRow
	meta map[string]string
}

func (f *fakeOverlayRepo) ReplaceCatalogOverlay(_ context.Context, rows []types.ExtCatalogOverlayRow, meta map[string]string) error {
	f.rows = rows
	if f.meta == nil {
		f.meta = map[string]string{}
	}
	for k, v := range meta {
		f.meta[k] = v
	}
	return nil
}

func (f *fakeOverlayRepo) GetCatalogOverlayMeta(_ context.Context, key string) (string, error) {
	return f.meta[key], nil
}

func (f *fakeOverlayRepo) ListCatalogOverlay(_ context.Context) (map[string]types.ExtCatalogOverlayRow, error) {
	return nil, nil
}

func TestOverlaySync_ETag(t *testing.T) {
	body, _ := json.Marshal(testOverlayIndex())
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write(body) //nolint:errcheck
	}))
	defer srv.Close()

	repo := &fakeOverlayRepo{}
	o := NewOverlaySync(repo, network.SafeHTTPClient{Client: srv.Client()}, srv.URL+"/index.json")
	n, err := o.Sync(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("first sync: n=%d err=%v", n, err)
	}
	if repo.meta[OverlayMetaETag] != `"v1"` || repo.meta[OverlayMetaTaxonomy] == "" {
		t.Fatalf("meta not stored: %+v", repo.meta)
	}
	n, err = o.Sync(context.Background())
	if err != nil || n != 0 || len(repo.rows) != 1 || hits != 2 {
		t.Fatalf("304 must keep data: n=%d err=%v rows=%d hits=%d", n, err, len(repo.rows), hits)
	}
}

func TestOverlaySync_RejectsHTTP(t *testing.T) {
	o := NewOverlaySync(&fakeOverlayRepo{}, network.SafeHTTPClient{Client: http.DefaultClient}, "http://example.com/index.json")
	if _, err := o.Sync(context.Background()); err == nil {
		t.Fatal("non-https url must be rejected")
	}
}
