//go:build linux

package probe

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCgroupFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestProbeCgroupMemory_ExcludesInactiveFile page cache 占满上限时，可回收的
// inactive_file 不得被算作已耗尽——此前可用量恒为 0，治理器长期判 L3。
func TestProbeCgroupMemory_ExcludesInactiveFile(t *testing.T) {
	const mb = 1 << 20
	cases := []struct {
		name     string
		v2, v1   map[string]string
		wantFree uint64
	}{
		{
			name: "v2",
			v2: map[string]string{
				"memory.max":     "2147483648",
				"memory.current": "2097152000",
				"memory.stat":    "anon 524288000\nfile 1572864000\ninactive_file 1048576000\nactive_file 524288000\n",
			},
			wantFree: 2147483648 - (2097152000 - 1048576000),
		},
		{
			name: "v1",
			v1: map[string]string{
				"memory.limit_in_bytes": "2147483648",
				"memory.usage_in_bytes": "2097152000",
				"memory.stat":           "cache 1572864000\ninactive_file 1\ntotal_inactive_file 1048576000\n",
			},
			wantFree: 2147483648 - (2097152000 - 1048576000),
		},
		{
			name: "v2 无 memory.stat 保守不扣除",
			v2: map[string]string{
				"memory.max":     "2147483648",
				"memory.current": "2097152000",
			},
			wantFree: 2147483648 - 2097152000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v2Dir, v1Dir := t.TempDir(), t.TempDir()
			writeCgroupFiles(t, v2Dir, tc.v2)
			writeCgroupFiles(t, v1Dir, tc.v1)
			limit, avail, ok := probeCgroupMemoryAt(v2Dir, v1Dir)
			if !ok || limit != 2048*mb {
				t.Fatalf("limit=%d ok=%v", limit, ok)
			}
			if avail != tc.wantFree {
				t.Fatalf("available=%d, want %d", avail, tc.wantFree)
			}
		})
	}
}
