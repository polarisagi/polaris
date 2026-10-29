package embedassets

import (
	"testing"
)

func TestManifestPlatforms(t *testing.T) {
	platforms := []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64"}
	for _, p := range platforms {
		a, ok := ORTLibAsset(p[:stringsIndex(p, "/")], p[stringsIndex(p, "/")+1:])
		if !ok {
			t.Fatalf("expected asset for platform %s", p)
		}
		if a.SHA256 == "" || a.Size <= 0 || a.URL == "" {
			t.Fatalf("invalid asset metadata for %s: %+v", p, a)
		}
	}

	_, ok := ORTLibAsset("unsupported", "arch")
	if ok {
		t.Fatalf("expected unsupported platform to return false")
	}
}

func TestGemmaAssets(t *testing.T) {
	assets := GemmaAssets()
	if len(assets) != 3 {
		t.Fatalf("expected 3 assets for Gemma, got %d", len(assets))
	}
	for _, a := range assets {
		if a.SHA256 == "" || a.Size <= 0 || a.URL == "" {
			t.Fatalf("invalid Gemma asset: %+v", a)
		}
	}
}

func TestBGEAssets(t *testing.T) {
	assets := BGEAssets()
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets for BGE, got %d", len(assets))
	}
	for _, a := range assets {
		if a.SHA256 == "" || a.Size <= 0 || a.URL == "" {
			t.Fatalf("invalid BGE asset: %+v", a)
		}
	}
}

func stringsIndex(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
