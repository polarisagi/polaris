package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

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
	ddl, err := schema.FS.ReadFile("011_providers.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply 011_providers.sql: %v", err)
	}
	return db
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
