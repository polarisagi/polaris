package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

func newVaultRotateTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	// 轮换覆盖全部 Vault 密文列（vaultCipherColumns），所涉表都要建。
	for _, f := range []string{"011_providers.sql", "015_mcp_servers.sql", "021_plugins.sql", "040_mcp_oauth.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

// TestNewVaultMasterKeyRotator_ReencryptsExtensionSecrets 插件敏感配置、MCP OAuth 令牌/客户端密钥、
// mcp_servers.oauth 内嵌预注册密钥在轮换后都能用新 key 解密（此前轮换只覆盖 providers）。
func TestNewVaultMasterKeyRotator_ReencryptsExtensionSecrets(t *testing.T) {
	dataDir := t.TempDir()
	oldVault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	db := newVaultRotateTestDB(t)
	enc := func(s string) string {
		out, err := oldVault.Encrypt(s)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	mustExec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO plugin_user_config(plugin_id, scope, key, value, sensitive) VALUES('pl_1','','token',?,1)`, enc(`"plugin-secret"`))
	mustExec(`INSERT INTO plugin_user_config(plugin_id, scope, key, value, sensitive) VALUES('pl_1','','region','"eu"',0)`)
	mustExec(`INSERT INTO mcp_oauth_clients(issuer, redirect_uri, client_id, client_secret_enc) VALUES('https://as','https://cb','c1',?)`, enc("dcr-secret"))
	mustExec(`INSERT INTO mcp_oauth_tokens(server_id, issuer, access_token_enc, refresh_token_enc) VALUES('s1','https://as',?,?)`, enc("access"), enc("refresh"))
	mustExec(`INSERT INTO mcp_servers(id, name, oauth) VALUES('s1','s1',?)`, `{"client_id":"pre","client_secret_enc":"`+enc("pre-secret")+`"}`)

	if _, err := newVaultMasterKeyRotator(db, oldVault, dataDir)(context.Background()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	newVault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	check := func(q, want string) {
		t.Helper()
		var ct string
		if err := db.QueryRow(q).Scan(&ct); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		got, err := newVault.Decrypt(ct)
		if err != nil || got != want {
			t.Errorf("%s: want %q after rotation, got %q (err %v)", q, want, got, err)
		}
	}
	check(`SELECT value FROM plugin_user_config WHERE key='token'`, `"plugin-secret"`)
	check(`SELECT client_secret_enc FROM mcp_oauth_clients`, "dcr-secret")
	check(`SELECT access_token_enc FROM mcp_oauth_tokens`, "access")
	check(`SELECT refresh_token_enc FROM mcp_oauth_tokens`, "refresh")
	var region string
	if err := db.QueryRow(`SELECT value FROM plugin_user_config WHERE key='region'`).Scan(&region); err != nil || region != `"eu"` {
		t.Errorf("non-sensitive config must stay untouched, got %q", region)
	}
	var raw string
	if err := db.QueryRow(`SELECT oauth FROM mcp_servers WHERE id='s1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	cfg, err := mcp.ParseRowOAuthConfig(raw)
	if err != nil || cfg == nil {
		t.Fatalf("parse oauth: %v", err)
	}
	if got, err := newVault.Decrypt(cfg.ClientSecretEnc); err != nil || got != "pre-secret" || cfg.ClientID != "pre" {
		t.Errorf("mcp_servers.oauth secret not re-encrypted: %q %v", got, err)
	}
}

// TestNewVaultMasterKeyRotator_RotatesAndSwapsKey 验证轮换后：
//  1. providers 表里的密文用新 key 能正确解密回原文；
//  2. 旧 key 解不出正确明文（证明确实换了 key，不是空操作）；
//  3. vault.key.new 临时文件被原子替换为 vault.key，不遗留。
func TestNewVaultMasterKeyRotator_RotatesAndSwapsKey(t *testing.T) {
	dataDir := t.TempDir()
	oldVault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	db := newVaultRotateTestDB(t)
	ctx := context.Background()

	pr := repo.NewSQLiteProviderRepository(db).WithVault(oldVault)
	if err := pr.UpsertProvider(ctx, types.ProviderRow{
		ID: "p1", Name: "test", Type: "openai_compat", APIKey: "sk-old-secret",
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	rotate := newVaultMasterKeyRotator(db, oldVault, dataDir)
	rotated, err := rotate(ctx)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated != 1 {
		t.Errorf("expected 1 provider rotated, got %d", rotated)
	}

	if _, err := os.Stat(filepath.Join(dataDir, "vault.key.new")); !os.IsNotExist(err) {
		t.Errorf("vault.key.new must not remain after successful rotation")
	}

	newVault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		t.Fatalf("reload vault after rotation: %v", err)
	}
	rows, err := repo.NewSQLiteProviderRepository(db).WithVault(newVault).ListProviders(ctx)
	if err != nil {
		t.Fatalf("list with new vault: %v", err)
	}
	if len(rows) != 1 || rows[0].APIKey != "sk-old-secret" {
		t.Errorf("new vault must decrypt to original plaintext, got %+v", rows)
	}

	staleRows, err := repo.NewSQLiteProviderRepository(db).WithVault(oldVault).ListProviders(ctx)
	if err != nil {
		t.Fatalf("list with old vault: %v", err)
	}
	if len(staleRows) == 1 && staleRows[0].APIKey == "sk-old-secret" {
		t.Errorf("old vault must NOT be able to decrypt post-rotation ciphertext")
	}
}

// TestNewVaultMasterKeyRotator_NoProviders 空库场景下轮换仍应成功（0 条记录），且正常换 key。
func TestNewVaultMasterKeyRotator_NoProviders(t *testing.T) {
	dataDir := t.TempDir()
	oldVault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	db := newVaultRotateTestDB(t)

	rotate := newVaultMasterKeyRotator(db, oldVault, dataDir)
	rotated, err := rotate(context.Background())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated != 0 {
		t.Errorf("expected 0 providers rotated, got %d", rotated)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "vault.key.new")); !os.IsNotExist(err) {
		t.Errorf("vault.key.new must not remain after successful rotation")
	}
}
