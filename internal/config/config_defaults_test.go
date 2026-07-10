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

	checkStructCoverage(t, reflect.TypeOf(SystemConfig{}), parsed["system"], whitelist, "system")
	checkStructCoverage(t, reflect.TypeOf(SandboxConfig{}), parsed["sandbox"], whitelist, "sandbox")
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
