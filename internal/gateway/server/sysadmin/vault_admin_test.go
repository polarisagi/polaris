package sysadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security/credential"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/sysmgr/updater"
	"github.com/polarisagi/polaris/pkg/types"
)

func newVaultTestDB(t *testing.T) *sql.DB {
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

func newVaultTestHandler(t *testing.T) (*SysAdminHandler, *sql.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	vault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	db := newVaultTestDB(t)
	return &SysAdminHandler{
		DataDir: dataDir,
		Vault:   vault,
		RWDB:    db,
	}, db, dataDir
}

// TestHandleVaultRotateMasterKey_Unauthenticated 验证未认证请求被拒绝，不触碰 vault.key。
func TestHandleVaultRotateMasterKey_Unauthenticated(t *testing.T) {
	h, _, dataDir := newVaultTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/vault/rotate-master-key", nil)
	w := httptest.NewRecorder()
	h.HandleVaultRotateMasterKey(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("unauthenticated: expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "vault.key.new")); !os.IsNotExist(err) {
		t.Errorf("unauthenticated request must not leave a half-written vault.key.new")
	}
}

// TestHandleVaultRotateMasterKey_RotatesAndSwapsKey 验证轮换后：
//  1. providers 表里的密文用新 key 能正确解密回原文；
//  2. 旧 key 解不出正确明文（证明确实换了 key，不是空操作）；
//  3. vault.key.new 临时文件被原子替换为 vault.key，不遗留。
func TestHandleVaultRotateMasterKey_RotatesAndSwapsKey(t *testing.T) {
	h, db, dataDir := newVaultTestHandler(t)
	oldVault := h.Vault

	pr := storerepo.NewSQLiteProviderRepository(db).WithVault(oldVault)
	ctx := context.Background()
	if err := pr.UpsertProvider(ctx, types.ProviderRow{
		ID: "p1", Name: "test", Type: "openai_compat", APIKey: "sk-old-secret",
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/vault/rotate-master-key", nil)
	authCtx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "local", ClientType: authcontext.ClientTypeLocal, Authenticated: true})
	req = req.WithContext(authCtx)
	w := httptest.NewRecorder()
	h.HandleVaultRotateMasterKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Status           string `json:"status"`
		ProvidersRotated int    `json:"providers_rotated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Status != "rotated" || resp.ProvidersRotated != 1 {
		t.Errorf("unexpected response: %+v", resp)
	}

	if _, err := os.Stat(filepath.Join(dataDir, "vault.key.new")); !os.IsNotExist(err) {
		t.Errorf("vault.key.new must not remain after successful rotation")
	}

	newVault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		t.Fatalf("reload vault after rotation: %v", err)
	}
	rows, err := storerepo.NewSQLiteProviderRepository(db).WithVault(newVault).ListProviders(ctx)
	if err != nil {
		t.Fatalf("list with new vault: %v", err)
	}
	if len(rows) != 1 || rows[0].APIKey != "sk-old-secret" {
		t.Errorf("new vault must decrypt to original plaintext, got %+v", rows)
	}

	staleRows, err := storerepo.NewSQLiteProviderRepository(db).WithVault(oldVault).ListProviders(ctx)
	if err != nil {
		t.Fatalf("list with old vault: %v", err)
	}
	if len(staleRows) == 1 && staleRows[0].APIKey == "sk-old-secret" {
		t.Errorf("old vault must NOT be able to decrypt post-rotation ciphertext")
	}
}

// TestHandleVaultRotateMasterKey_TriggersRestart 验证轮换成功后异步触发 Updater.Restart()。
func TestHandleVaultRotateMasterKey_TriggersRestart(t *testing.T) {
	h, _, _ := newVaultTestHandler(t)
	restarted := make(chan struct{}, 1)
	m := updater.New("dev", "", "", nil)
	m.SetRestartFn(func() { restarted <- struct{}{} })
	h.Updater = m

	req := httptest.NewRequest(http.MethodPost, "/v1/vault/rotate-master-key", nil)
	authCtx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "local", ClientType: authcontext.ClientTypeLocal, Authenticated: true})
	req = req.WithContext(authCtx)
	w := httptest.NewRecorder()
	h.HandleVaultRotateMasterKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("restart was not triggered")
	}
}
