package audiorun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/llm/audioassets"
	"github.com/polarisagi/polaris/internal/llm/stt"
)

func writeFile(t *testing.T, dir, rel string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func libFileName() string { return stt.LibName() }

func stubLib() (audioassets.Asset, error) { return stt.LibAssetForHost() }
