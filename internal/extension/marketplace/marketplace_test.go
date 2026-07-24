package marketplace

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/internal/security/network"
)

type mockRoundTripperFunc func(req *http.Request) *http.Response

func (f mockRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func TestMCPMarketplaceClient_Search(t *testing.T) {
	mockResp := mcpRegistryResponse{
		Servers: []mcpRegistryServer{
			{
				Server: mcpServerDef{
					Name:        "testpub/testpkg",
					Description: "Test Package",
					Version:     "1.0",
					Repository:  mcpRepository{URL: "https://repo.com"},
					Remotes: []mcpRemoteDef{
						{Type: "stdio", URL: "https://dl.com"},
					},
				},
			},
		},
	}

	clientHTTP := network.NewSafeHTTPClient(nil)
	clientHTTP.Transport = mockRoundTripperFunc(func(req *http.Request) *http.Response {
			if req.URL.Path != "/servers" {
				t.Errorf("expected /servers, got %s", req.URL.Path)
			}
			if req.URL.Query().Get("search") != "test" {
				t.Errorf("expected query 'test', got %s", req.URL.Query().Get("search"))
			}
			b, _ := json.Marshal(mockResp)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(string(b))),
			}
		})

	client := NewMCPMarketplaceClient("http://dummy", "", clientHTTP)
	entries, err := client.Search(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.ID != "testpub/testpkg" || e.Name != "testpub/testpkg" || e.Publisher != "testpub" || e.Transport != "stdio" || e.URL != "https://dl.com" {
		t.Errorf("unexpected entry: %+v", e)
	}
}

func TestMCPMarketplaceClient_Search_Error(t *testing.T) {
	clientHTTP := network.NewSafeHTTPClient(nil)
	clientHTTP.Transport = mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("")),
			}
		})

	client := NewMCPMarketplaceClient("http://dummy", "", clientHTTP)
	_, err := client.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMCPMarketplaceClient_Install_Stdio(t *testing.T) {
	dir := t.TempDir()
	client := NewMCPMarketplaceClient("", dir, network.NewSafeHTTPClient(nil))

	pkg := protocol.RegistryEntry{
		ID:          "test/pkg",
		Name:        "test_pkg",
		Command:     "test_cmd",
		Description: "Test Desc",
	}

	outDir, err := client.Install(context.Background(), pkg)
	if err != nil {
		t.Fatal(err)
	}

	expectedDir := filepath.Join(dir, "test_pkg")
	if outDir != expectedDir {
		t.Errorf("expected %s, got %s", expectedDir, outDir)
	}

	mcpJSONPath := filepath.Join(outDir, ".mcp.json")
	if _, err := os.Stat(mcpJSONPath); err != nil {
		t.Errorf("missing .mcp.json: %v", err)
	}
	pluginJSONPath := filepath.Join(outDir, ".polaris-plugin", "plugin.json")
	if _, err := os.Stat(pluginJSONPath); err != nil {
		t.Errorf("missing plugin.json: %v", err)
	}
}

func TestMCPMarketplaceClient_Install_HTTP(t *testing.T) {
	dir := t.TempDir()
	client := NewMCPMarketplaceClient("", dir, network.NewSafeHTTPClient(nil))

	pkg := protocol.RegistryEntry{
		ID:        "test/pkg",
		Name:      "test_pkg",
		Transport: "http",
		URL:       "http://localhost:8080/mcp",
	}

	outDir, err := client.Install(context.Background(), pkg)
	if err != nil {
		t.Fatal(err)
	}

	mcpJSONPath := filepath.Join(outDir, ".mcp.json")
	data, err := os.ReadFile(mcpJSONPath)
	if err != nil {
		t.Fatal(err)
	}

	var cfg protocol.MCPConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	server, ok := cfg.MCPServers["test_pkg"]
	if !ok {
		t.Fatal("missing test_pkg server")
	}
	if server.Type != "http" || server.URL != "http://localhost:8080/mcp" {
		t.Errorf("unexpected server def: %+v", server)
	}
}

func TestMCPMarketplaceClient_Install_Download(t *testing.T) {
	clientHTTP := network.NewSafeHTTPClient(nil)
	clientHTTP.Transport = mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("binary data")),
			}
		})

	dir := t.TempDir()
	client := NewMCPMarketplaceClient("", dir, clientHTTP)

	pkg := protocol.RegistryEntry{
		ID:      "test/download",
		Name:    "test_download",
		Command: "test_bin",
		URL:     "http://dummy/download",
		Checksum: "9cb63cb779e8c571db3199b783a36cc43cd9e7c076beeb496c39e9cc06196dc5", // will trigger download
	}

	outDir, err := client.Install(context.Background(), pkg)
	if err != nil {
		t.Fatal(err)
	}

	binPath := filepath.Join(outDir, "test_bin")
	data, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "binary data" {
		t.Errorf("unexpected binary content: %s", string(data))
	}
}

func TestMCPMarketplaceClient_Install_ChecksumVerification(t *testing.T) {
	clientHTTP := network.NewSafeHTTPClient(nil)
	clientHTTP.Transport = mockRoundTripperFunc(func(req *http.Request) *http.Response {
			if req.URL.Path == "/checksums.txt" {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("9cb63cb779e8c571db3199b783a36cc43cd9e7c076beeb496c39e9cc06196dc5  test_bin\n")),
				}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("binary data")),
			}
		})

	client := NewMCPMarketplaceClient("", t.TempDir(), clientHTTP)

	// 1. Checksum matched
	pkg1 := protocol.RegistryEntry{
		ID:        "test/checksum_pass",
		Name:      "test_pass",
		Command:   "test_bin",
		URL:       "http://dummy/bin",
		Checksum:  "9cb63cb779e8c571db3199b783a36cc43cd9e7c076beeb496c39e9cc06196dc5",
		TrustTier: int(types.TrustOfficial),
	}
	_, err := client.Install(context.Background(), pkg1)
	if err != nil {
		t.Errorf("expected pass, got: %v", err)
	}

	// 2. Checksum mismatched (reject and delete)
	pkg2 := protocol.RegistryEntry{
		ID:        "test/checksum_fail",
		Name:      "test_fail",
		Command:   "test_bin",
		URL:       "http://dummy/bin",
		Checksum:  "badhex",
		TrustTier: int(types.TrustOfficial),
	}
	outDir, err := client.Install(context.Background(), pkg2)
	if err == nil {
		t.Error("expected checksum mismatch error, got nil")
	}
	if _, err := os.Stat(filepath.Join(outDir, "test_bin")); !os.IsNotExist(err) {
		t.Error("expected file to be deleted on mismatch")
	}

	// 3. Official missing checksum (error)
	pkg3 := protocol.RegistryEntry{
		ID:        "test/checksum_missing_official",
		Name:      "test_official",
		Command:   "test_bin",
		URL:       "http://dummy/bin",
		TrustTier: int(types.TrustOfficial),
	}
	_, err = client.Install(context.Background(), pkg3)
	if err == nil || !strings.Contains(err.Error(), "missing checksum") {
		t.Errorf("expected missing checksum error for official, got: %v", err)
	}

	// 4. Community missing checksum (rejected)
	pkg4 := protocol.RegistryEntry{
		ID:        "test/checksum_missing_community",
		Name:      "test_community",
		Command:   "test_bin",
		URL:       "http://dummy/bin",
		TrustTier: int(types.TrustCommunity),
	}
	_, err = client.Install(context.Background(), pkg4)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("expected rejection for community missing checksum, got: %v", err)
	}

	// 5. Fetch checksum from URL
	pkg5 := protocol.RegistryEntry{
		ID:          "test/checksum_url_pass",
		Name:        "test_url",
		Command:     "test_bin",
		URL:         "http://dummy/bin",
		ChecksumURL: "http://dummy/checksums.txt",
		TrustTier:   int(types.TrustOfficial),
	}
	_, err = client.Install(context.Background(), pkg5)
	if err != nil {
		t.Errorf("expected pass from checksum URL, got: %v", err)
	}
}
