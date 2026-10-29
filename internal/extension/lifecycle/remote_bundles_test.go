package lifecycle

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// fakeDownloader 按 URL 写入预置的 .mcpb 内容；未登记的 URL 视为下载失败。
type fakeDownloader map[string]map[string]string

func (f fakeDownloader) DownloadTo(_ context.Context, rawURL, dest string) error {
	files, ok := f[rawURL]
	if !ok {
		return apperr.New(apperr.CodeNetworkUnavailable, "unreachable")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return err
		}
	}
	return zw.Close()
}

func TestPluginInstaller_RemoteBundles(t *testing.T) {
	extRepo := newTestExtRepo(t)
	root := filepath.Join(t.TempDir(), "rb")
	writeTestFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"rb","mcpServers":[
	  "https://cdn.example/ok.mcpb","https://cdn.example/missing.mcpb"]}`)
	dl := fakeDownloader{"https://cdn.example/ok.mcpb": {
		"manifest.json": `{"name":"remote","server":{"mcp_config":{"command":"node","args":["${__dirname}/s.js"]}}}`,
		"s.js":          "//",
	}}
	inst := NewPluginInstaller(extRepo, &recordingConnector{}, &recordingSkillRegistry{}).WithPolicyGate(allowAllGate{}).WithRemoteDownloader(dl)
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_rb", LocalPath: root}); err != nil {
		t.Fatal(err)
	}
	servers, err := extRepo.ListMCPServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Command != "node" {
		t.Fatalf("fetched bundle must register its server; unreachable one must not: %+v", servers)
	}
}
