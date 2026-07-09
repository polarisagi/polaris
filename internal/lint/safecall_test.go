package lint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func Test_inv_NoBareLLMInfer(t *testing.T) {
	inferRe := regexp.MustCompile(`\.Infer\s*\(|\.StreamInfer\s*\(`)

	err := filepath.Walk("../", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "vendor" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		p := filepath.ToSlash(path)
		// 豁免包
		if strings.HasPrefix(p, "../llm/") || strings.HasPrefix(p, "../protocol/") || strings.HasPrefix(p, "../lint/") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		if inferRe.Match(content) {
			lines := strings.Split(string(content), "\n")
			for i, line := range lines {
				if inferRe.MatchString(line) {
					// 忽略注释
					if strings.HasPrefix(strings.TrimSpace(line), "//") || strings.Contains(line, "safecall.Infer") || strings.Contains(line, "safecall.StreamInfer") {
						continue
					}
					if i > 0 && strings.Contains(lines[i-1], "custom-nolint:bare-infer") {
						continue
					}
					t.Errorf("VIOLATED: %s:%d %s (禁止裸调用 provider.Infer/StreamInfer，请使用 internal/llm/safecall)", path, i+1, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})

	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}
}
