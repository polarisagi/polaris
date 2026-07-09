package main

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func newTestSystemRepo(t *testing.T) *repo.SQLiteSystemRepository {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS preferences (
		key TEXT PRIMARY KEY, value TEXT NOT NULL, expired_at INTEGER
	)`); err != nil {
		t.Fatalf("create preferences table: %v", err)
	}
	return repo.NewSQLiteSystemRepository(db)
}

// TestResolveNotionToken_EnvFallbackThenPersisted 验证 Notion token 解析优先级
// （P2-4 vault 加固）：preferences 为空时回退 NOTION_TOKEN env，并把加密后的
// 值写回 preferences；随后的调用即便不再依赖 env 也能从 vault 正确解密取回。
func TestResolveNotionToken_EnvFallbackThenPersisted(t *testing.T) {
	sysRepo := newTestSystemRepo(t)
	vault, err := credential.NewVaultWithKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewVaultWithKey: %v", err)
	}

	t.Setenv("NOTION_TOKEN", "secret-notion-token")

	token, err := resolveNotionToken(context.Background(), sysRepo, vault)
	if err != nil {
		t.Fatalf("resolveNotionToken failed: %v", err)
	}
	if token != "secret-notion-token" {
		t.Errorf("expected token from env, got %q", token)
	}

	// 确认已加密落盘（而非明文）。
	stored, err := sysRepo.GetPreference(context.Background(), notionTokenPrefKey)
	if err != nil {
		t.Fatalf("GetPreference: %v", err)
	}
	if stored == "" || stored == "secret-notion-token" {
		t.Errorf("expected encrypted value persisted, got raw %q", stored)
	}

	// 清空 env，验证第二次调用完全依赖已落盘的加密值，不再需要 env。
	t.Setenv("NOTION_TOKEN", "")
	token2, err := resolveNotionToken(context.Background(), sysRepo, vault)
	if err != nil {
		t.Fatalf("resolveNotionToken (second call) failed: %v", err)
	}
	if token2 != "secret-notion-token" {
		t.Errorf("expected token decrypted from vault store, got %q", token2)
	}
}

// TestResolveNotionToken_NoneConfigured 验证 env 和 vault 均未配置时返回明确错误，
// 而不是静默返回空字符串（会导致 NotionConnector 用空 token 静默失败于更深处）。
func TestResolveNotionToken_NoneConfigured(t *testing.T) {
	sysRepo := newTestSystemRepo(t)
	vault, err := credential.NewVaultWithKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewVaultWithKey: %v", err)
	}
	t.Setenv("NOTION_TOKEN", "")

	if _, err := resolveNotionToken(context.Background(), sysRepo, vault); err == nil {
		t.Errorf("expected error when neither env nor vault has a token")
	}
}
