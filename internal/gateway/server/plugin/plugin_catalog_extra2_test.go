package plugin

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type dummyPolicyGate struct{}

func (d *dummyPolicyGate) IsAuthorized(ctx context.Context, principal, action, resource string, context map[string]any) (bool, error) {
	return true, nil
}
func (d *dummyPolicyGate) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

type dummyPreferences struct{}

func (d *dummyPreferences) GetPermissionMode(ctx context.Context) (types.PermissionMode, error) {
	return types.ModeDefault, nil
}
func (d *dummyPreferences) SetPermissionMode(ctx context.Context, mode types.PermissionMode) error {
	return nil
}

func getDummyServerWithInstallMgr(t *testing.T) *PluginHandler {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	mgr := marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, &dummyPolicyGate{}, &dummyPreferences{}, nil, nil)
	return &PluginHandler{DB: db, InstallMgr: mgr}
}

func TestInternalInstallMCP(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	_, err := h.internalInstallMCP(context.Background(), "ext1", &protocol.RegistryEntry{}, protocol.PluginInstallRequest{}, "now")
	if err == nil {
		t.Errorf("expected error due to missing schema")
	}
}

func TestInstallMCPExtension(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	req := httptest.NewRequest("POST", "/install", nil)
	w := httptest.NewRecorder()
	h.installMCPExtension(w, req, "ext1", &protocol.RegistryEntry{}, protocol.PluginInstallRequest{}, "now")
}

func TestInternalInstallGeneric(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	_, err := h.internalInstallGeneric(context.Background(), "ext1", &protocol.RegistryEntry{}, protocol.PluginInstallRequest{}, "now")
	if err == nil {
		t.Errorf("expected error due to missing schema")
	}
}

func TestInstallGenericExtension(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	req := httptest.NewRequest("POST", "/install", nil)
	w := httptest.NewRecorder()
	h.installGenericExtension(w, req, "ext1", &protocol.RegistryEntry{}, protocol.PluginInstallRequest{}, "now")
}

func TestDownloadAndInstallExtension(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	// It triggers asynchronous or synchronous download operations. We just want it to not panic.
	h.downloadAndInstallExtension(context.Background(), "ext1", "cat1", &protocol.RegistryEntry{}, "now", "name")
}

func TestUpdateExtensionInstanceError(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	h.updateExtensionInstanceError(context.Background(), "ext1", "error test")
}
