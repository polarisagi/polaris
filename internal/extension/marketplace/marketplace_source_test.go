package marketplace

import "testing"

func TestNormalizeMarketplaceSource(t *testing.T) {
	dir := t.TempDir()
	ok := map[[2]string]string{
		{"org/mkt", "plugin"}:                                     "https://github.com/org/mkt.git",
		{"org/mkt@v2", "plugin"}:                                  "https://github.com/org/mkt.git#v2",
		{"https://git.example/m.git#main", "plugin"}:              "https://git.example/m.git#main",
		{"https://x.example/marketplace.json", "plugin"}:          "https://x.example/marketplace.json",
		{dir, "skill"}:                                            dir,
		{"https://registry.modelcontextprotocol.io/v0.1/", "mcp"}: "https://registry.modelcontextprotocol.io/v0.1",
	}
	for in, want := range ok {
		if got, err := NormalizeMarketplaceSource(in[0], in[1]); err != nil || got != want {
			t.Errorf("%v: got %q %v want %q", in, got, err, want)
		}
	}
	for _, bad := range [][2]string{{"", "plugin"}, {"git@github.com:o/r.git", "plugin"}, {"http://x/y", "plugin"},
		{"file:///etc", "plugin"}, {"/definitely/missing/dir", "plugin"}, {"justname", "plugin"}, {"org/mkt", "mcp"}} {
		if _, err := NormalizeMarketplaceSource(bad[0], bad[1]); err == nil {
			t.Errorf("%v must be rejected", bad)
		}
	}
	if u, ref := splitRef("https://github.com/o/r.git#v1"); u != "https://github.com/o/r.git" || ref != "v1" {
		t.Fatalf("splitRef: %s %s", u, ref)
	}
}
