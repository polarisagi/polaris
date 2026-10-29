package read_file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLines(t *testing.T, n int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func call(t *testing.T, dir string, args map[string]any) string {
	t.Helper()
	in, _ := json.Marshal(args)
	out, err := MakeReadFileFn([]string{dir})(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestReadFile_NumberedAndWhole(t *testing.T) {
	dir, p := writeLines(t, 3)
	got := call(t, dir, map[string]any{"path": p})
	if got != "1\tline 1\n2\tline 2\n3\tline 3\n" {
		t.Fatalf("got %q", got)
	}
}

func TestReadFile_Range(t *testing.T) {
	dir, p := writeLines(t, 100)
	got := call(t, dir, map[string]any{"path": p, "offset": 10, "limit": 3})
	if !strings.HasPrefix(got, "10\tline 10\n11\tline 11\n12\tline 12\n") || !strings.Contains(got, "showing lines 10-12 of 100") {
		t.Fatalf("got %q", got)
	}
}

func TestReadFile_DefaultCap(t *testing.T) {
	dir, p := writeLines(t, defaultMaxLines+50)
	got := call(t, dir, map[string]any{"path": p})
	if !strings.Contains(got, fmt.Sprintf("showing lines 1-%d of %d", defaultMaxLines, defaultMaxLines+50)) {
		t.Fatalf("应注明截断与总行数: %q", got[len(got)-120:])
	}
}

func TestReadFile_OffsetPastEnd(t *testing.T) {
	dir, p := writeLines(t, 5)
	if got := call(t, dir, map[string]any{"path": p, "offset": 50}); !strings.Contains(got, "has 5 lines") {
		t.Fatalf("got %q", got)
	}
}
