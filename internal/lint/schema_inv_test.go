package lint_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Test_inv_SchemaFileNamingConvention 验证 Schema 文件命名规范。
func Test_inv_SchemaFileNamingConvention(t *testing.T) {
	root := repoRoot(t)
	schemaDir := filepath.Join(root, "internal", "protocol", "schema")

	files, err := os.ReadDir(schemaDir)
	if err != nil {
		t.Fatalf("read schema dir: %v", err)
	}

	namePattern := regexp.MustCompile(`^(\d{3})_[a-z_]+\.sql$`)
	seenNumbers := make(map[string]string)

	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".sql" {
			continue
		}

		matches := namePattern.FindStringSubmatch(file.Name())
		if matches == nil {
			t.Errorf("inv_SchemaFileNamingConvention VIOLATED: %s 文件名不符合 NNN_name.sql 规范", file.Name())
			continue
		}

		numStr := matches[1]
		if numStr == "025" || numStr == "026" || numStr == "027" {
			t.Errorf("inv_SchemaFileNamingConvention VIOLATED: 编号 %s 被预留，禁止使用 (%s)", numStr, file.Name())
		}

		if prev, exists := seenNumbers[numStr]; exists {
			t.Errorf("inv_SchemaFileNamingConvention VIOLATED: 发现重复编号 %s (%s 和 %s)", numStr, prev, file.Name())
		}
		seenNumbers[numStr] = file.Name()
	}
}
