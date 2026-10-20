package webui

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 前端 i18n 的 t(key) 缺 key 时返回 key 本身，`t('x') || '兜底'` 的兜底永远不会生效，
// 用户看到的是裸 key（"chat_recall"）。本测试把两类错误挡在 CI：
//  1. 源码里字面量 key 在 zh 或 en 词条表里缺失；
//  2. 再次出现 `t(...) ||` / `t(...) ??` 这种形同虚设的兜底写法。
var (
	i18nKeyDef  = regexp.MustCompile(`(?m)^\s*['"]?([A-Za-z0-9_]+)['"]?\s*:`)
	i18nKeyUse  = regexp.MustCompile(`\bt\(\s*['"]([A-Za-z0-9_]+)['"]\s*[,)]`)
	i18nDynUse  = regexp.MustCompile(`\bt\(\s*['"]([A-Za-z0-9_]+_)['"]\s*\+`)
	i18nFallbck = regexp.MustCompile(`i18n\.t\([^)]*\)\s*(\|\||\?\?)`)
)

func isFrontendSource(path string) bool {
	return strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".html")
}

func loadI18nKeys(t *testing.T) (zh, en map[string]bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("src", "js", "i18n.js"))
	if err != nil {
		t.Fatalf("读取 i18n.js: %v", err)
	}
	src := string(raw)
	cut := strings.Index(src, "export const en")
	if cut < 0 {
		t.Fatal("i18n.js 中找不到 export const en")
	}
	collect := func(block string) map[string]bool {
		m := map[string]bool{}
		for _, sub := range i18nKeyDef.FindAllStringSubmatch(block, -1) {
			m[sub[1]] = true
		}
		return m
	}
	return collect(src[:cut]), collect(src[cut:])
}

func TestI18nKeysUsedInSourceExistInBothLanguages(t *testing.T) {
	zh, en := loadI18nKeys(t)
	err := filepath.WalkDir("src", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isFrontendSource(path) || filepath.Base(path) == "i18n.js" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(b)
		for _, sub := range i18nKeyUse.FindAllStringSubmatch(text, -1) {
			k := sub[1]
			if !zh[k] {
				t.Errorf("%s: key %q 在 zh 词条表中缺失", path, k)
			}
			if !en[k] {
				t.Errorf("%s: key %q 在 en 词条表中缺失", path, k)
			}
		}
		if loc := i18nFallbck.FindString(text); loc != "" {
			t.Errorf("%s: %q — t() 缺 key 返回 key 本身，`||` 兜底不会生效；请补词条而不是写兜底", path, loc)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 动态拼接的 key（'step_kind_' + kind 等）至少要求前缀下有词条，且 zh/en 条目集合一致。
func TestI18nDynamicPrefixesCoveredInBothLanguages(t *testing.T) {
	zh, en := loadI18nKeys(t)
	prefixes := map[string]bool{}
	walkErr := filepath.WalkDir("src", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isFrontendSource(path) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, sub := range i18nDynUse.FindAllStringSubmatch(string(b), -1) {
			prefixes[sub[1]] = true
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	for p := range prefixes {
		for k := range zh {
			if strings.HasPrefix(k, p) && !en[k] {
				t.Errorf("动态前缀 %q：zh 有 %q 而 en 缺失", p, k)
			}
		}
		for k := range en {
			if strings.HasPrefix(k, p) && !zh[k] {
				t.Errorf("动态前缀 %q：en 有 %q 而 zh 缺失", p, k)
			}
		}
	}
}
