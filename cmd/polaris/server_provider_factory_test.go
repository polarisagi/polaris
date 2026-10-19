package main

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// TestDBProviderFactory_CreateProvider_LocalOnlyMode 验证 dbProviderFactory.CreateProvider
// 在 local_only_mode 下拒绝实例化云端 Provider（V3 / HE-2）。
func TestDBProviderFactory_CreateProvider_LocalOnlyMode(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE providers (
			id TEXT PRIMARY KEY,
			name TEXT,
			type TEXT,
			base_url TEXT,
			api_key TEXT,
			project_id TEXT,
			location TEXT,
			sa_key_json TEXT,
			enabled INTEGER,
			is_local INTEGER DEFAULT 0
		);
		INSERT INTO providers (id, name, type, base_url, api_key, enabled, is_local)
		VALUES ('p_openai', 'OpenAI', 'openai_compat', 'https://api.openai.com', 'sk-xxx', 1, 0);
		INSERT INTO providers (id, name, type, base_url, api_key, enabled, is_local)
		VALUES ('p_ollama', 'Ollama', 'ollama', 'http://localhost:11434', '', 1, 1);
	`); err != nil {
		t.Fatal(err)
	}

	origCfg := config.Get()
	defer config.Update(origCfg)

	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.LocalOnlyMode = true
	config.Update(&newCfg)

	factory := NewDBProviderFactory(db, nil, &http.Client{}, nil)
	ctx := context.Background()

	// 1. 尝试创建云端 Provider：必须返回 CodeForbidden
	_, _, err = factory.CreateProvider(ctx, "p_openai", "gpt-4o")
	if err == nil {
		t.Fatal("V3 缺陷：CreateProvider 在 local_only_mode 下必须拒绝云端 Provider")
	}
	if apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("期望 CodeForbidden, 实际: %v", err)
	}
	if !strings.Contains(err.Error(), "local_only_mode") {
		t.Fatalf("期望包含 local_only_mode 错误信息, 实际: %v", err)
	}

	// 2. 尝试创建本地 Provider：允许通过（ollama）
	p, name, err := factory.CreateProvider(ctx, "p_ollama", "llama3")
	if err != nil {
		t.Fatalf("local_only_mode 下创建 ollama 本地 provider 失败: %v", err)
	}
	if p == nil || !strings.Contains(name, "llama3") {
		t.Fatalf("期望成功创建 ollama provider, 实际: p=%v, name=%s", p, name)
	}
}
