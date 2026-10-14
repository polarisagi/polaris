package pipeline_test

import (
	"go/build"
	"strings"
	"testing"
)

// TestPipelinePackage_ImportsWhitelist 确保 pipeline 纯逻辑包绝不引入外部 IO 与高级层依赖。
// 规则：只允许 Go 标准库、pkg/apperr 与 pkg/types；禁止 net/http, database/sql, internal/gateway, internal/tool 等。
func TestPipelinePackage_ImportsWhitelist(t *testing.T) {
	pkg, err := build.Import("github.com/polarisagi/polaris/internal/automation/pipeline", "", build.FindOnly)
	if err != nil {
		t.Fatalf("build.Import: %v", err)
	}

	loaded, err := build.ImportDir(pkg.Dir, 0)
	if err != nil {
		t.Fatalf("build.ImportDir: %v", err)
	}

	for _, imp := range loaded.Imports {
		if imp == "net/http" || imp == "database/sql" {
			t.Errorf("FORBIDDEN import: %s is strictly banned in pipeline logic package", imp)
		}
		if strings.HasPrefix(imp, "github.com/polarisagi/polaris/") {
			if imp != "github.com/polarisagi/polaris/pkg/apperr" &&
				imp != "github.com/polarisagi/polaris/pkg/types" {
				t.Errorf("FORBIDDEN internal import: %s (only pkg/apperr and pkg/types are allowed)", imp)
			}
		}
	}
}
