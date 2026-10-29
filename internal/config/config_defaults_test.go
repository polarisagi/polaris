package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestDefaultsTOML_Coverage(t *testing.T) {
	// 找到 configs/defaults.toml
	// 假设测试在 internal/config 下运行，项目根目录是 ../../
	defaultsPath := filepath.Join("..", "..", "configs", "defaults.toml")
	content, err := os.ReadFile(defaultsPath)
	if err != nil {
		t.Fatalf("Failed to read defaults.toml: %v", err)
	}

	var parsed map[string]any
	if err := toml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("Failed to parse defaults.toml: %v", err)
	}

	// 白名单：这些字段在 defaults.toml 中预期不存在或不需要强制对应
	whitelist := map[string]bool{
		// 可以在此处添加预期不在 defaults.toml 中的字段
	}

	// 遍历顶层 Config 的每个子模块字段（而非手工列举 2-3 个结构体），
	// 防止未来任何模块新增配置字段时再次遗漏同步 defaults.toml（GR-2-002 教训：
	// 此前仅检查 System/Sandbox 两个结构体，导致 Policy.CedarEnforceMode 新增
	// 字段未同步到 defaults.toml 却无测试能发现）。
	cfgType := reflect.TypeOf(Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		field := cfgType.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue // Thresholds 等显式声明不落盘的字段跳过
		}
		checkStructCoverage(t, field.Type, parsed[tag], whitelist, tag)
	}
}

func checkStructCoverage(t *testing.T, typ reflect.Type, data any, whitelist map[string]bool, prefix string) {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	dataMap, ok := data.(map[string]any)
	if !ok && data != nil {
		t.Errorf("Expected data for %s to be map[string]any, got %T", prefix, data)
		return
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}

		fullKey := prefix + "." + tag
		if whitelist[fullKey] {
			continue
		}

		if dataMap == nil {
			t.Errorf("Missing section in defaults.toml for %s (field %s)", prefix, tag)
			continue
		}

		val, exists := dataMap[tag]
		if !exists {
			t.Errorf("Missing field '%s' under [%s] in defaults.toml", tag, prefix)
			continue
		}

		// 如果字段是一个结构体，递归检查
		if field.Type.Kind() == reflect.Struct {
			checkStructCoverage(t, field.Type, val, whitelist, fullKey)
		}
	}
}
