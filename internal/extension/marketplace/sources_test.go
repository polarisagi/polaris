package marketplace

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type mapTransport map[string][]byte

func (m mapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, ok := m[req.URL.String()]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404", Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}

func testFetcher(t *testing.T, files mapTransport) *SourceFetcher {
	t.Helper()
	c := network.NewSafeHTTPClient(nil)
	c.Transport = files
	return NewSourceFetcher(c, t.TempDir())
}

func TestFetch_RelativeSkipsSymlinks(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".claude-plugin", "plugin.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "leak")); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := testFetcher(t, nil).Fetch(context.Background(), pluginspec.PluginSource{Type: pluginspec.SourceRelative, Path: src}, "", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatal("plugin files must be copied")
	}
	if _, err := os.Lstat(filepath.Join(dest, "leak")); err == nil {
		t.Fatal("symlinks must not be copied")
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetch_ArchiveChecksumAndNestedRoot(t *testing.T) {
	data := zipBytes(t, map[string]string{"fmt-2.0/.claude-plugin/plugin.json": `{"name":"fmt"}`, "fmt-2.0/skills/a/SKILL.md": "x"})
	sum := sha256.Sum256(data)
	f := testFetcher(t, mapTransport{"https://a.example/fmt.zip": data})
	src := pluginspec.PluginSource{Type: pluginspec.SourceArchive, URL: "https://a.example/fmt.zip", SHA256: strings.ToUpper(hex.EncodeToString(sum[:]))}
	dest := filepath.Join(t.TempDir(), "fmt")
	if _, err := f.Fetch(context.Background(), src, "", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatal("plugin root one level down must be unwrapped")
	}
	src.SHA256 = strings.Repeat("0", 64)
	if _, err := f.Fetch(context.Background(), src, "", filepath.Join(t.TempDir(), "bad")); err == nil {
		t.Fatal("sha256 mismatch must be refused")
	}
}

func tgzBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetch_NPMRangeAndIntegrity(t *testing.T) {
	tgz := tgzBytes(t, map[string]string{"package/plugin.json": `{"name":"fmt"}`})
	sum := sha512.Sum512(tgz)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	meta := `{"dist-tags":{"latest":"3.0.0"},"versions":{
	  "2.1.0":{"dist":{"tarball":"https://r.example/fmt-2.1.0.tgz","integrity":"` + integrity + `"}},
	  "2.2.0-beta.1":{"dist":{"tarball":"https://r.example/beta.tgz","integrity":"sha512-AA=="}},
	  "3.0.0":{"dist":{"tarball":"https://r.example/fmt-3.0.0.tgz","integrity":"sha512-AA=="}}}}`
	f := testFetcher(t, mapTransport{"https://r.example/@org%2Ffmt": []byte(meta), "https://r.example/fmt-2.1.0.tgz": tgz,
		"https://r.example/fmt-3.0.0.tgz": tgz})
	src := pluginspec.PluginSource{Type: pluginspec.SourceNPM, Package: "@org/fmt", Version: "^2.0.0", Registry: "https://r.example"}
	got, err := f.Fetch(context.Background(), src, "", filepath.Join(t.TempDir(), "npm"))
	if err != nil || got.Version != "2.1.0" {
		t.Fatalf("range must pick the highest non-prerelease match: %+v %v", got, err)
	}
	src.Version = ""
	if _, err := f.Fetch(context.Background(), src, "", filepath.Join(t.TempDir(), "latest")); err == nil {
		t.Fatal("integrity mismatch must be refused")
	}
	src.Registry = "https://user:pw@r.example"
	if _, err := f.Fetch(context.Background(), src, "", filepath.Join(t.TempDir(), "cred")); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("registry with credentials must be rejected: %v", err)
	}
}

type headerTransport struct {
	body []byte
	got  http.Header
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.got = req.Header.Clone()
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(h.body)), Request: req}, nil
}

func TestFetch_ArchiveSendsEntryHeaders(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "marketplace.json"), []byte(`{"name":"m","owner":{"name":"o"},"plugins":[
	  {"name":"z","source":{"source":"archive","url":"https://a.example/z.zip"},"headers":{"Authorization":"Bearer t","Host":"evil"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := pluginspec.GetMarketplace(root)
	if err != nil || len(m.Plugins) != 1 {
		t.Fatalf("%+v %v", m, err)
	}
	ht := &headerTransport{body: zipBytes(t, map[string]string{".claude-plugin/plugin.json": `{"name":"z"}`})}
	c := network.NewSafeHTTPClient(nil)
	c.Transport = ht
	if _, err := NewSourceFetcher(c, t.TempDir()).Fetch(context.Background(), m.Plugins[0].Source, "", filepath.Join(t.TempDir(), "z")); err != nil {
		t.Fatal(err)
	}
	if ht.got.Get("Authorization") != "Bearer t" || ht.got.Get("Host") == "evil" {
		t.Fatalf("entry headers must be sent, routing headers dropped: %v", ht.got)
	}
}
