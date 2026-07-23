package marketplace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/store/repo"
)

type mockHookRunner struct {
	calledPath string
}

func (m *mockHookRunner) RunHook(ctx context.Context, hookPath, workDir string) error {
	m.calledPath = hookPath
	return nil
}

func TestManager_UninstallExtension_HookPathTraversal(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	runner := &mockHookRunner{}
	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil).WithHookRunner(runner)
	ctx := context.Background()

	tmpDir := t.TempDir()
	pluginDir := filepath.Join(tmpDir, "plugin_a")
	err := os.MkdirAll(pluginDir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	pluginJSON := `{"hooks": {"uninstall": "../plugin_a_evil/hook.sh"}}`
	err = os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(pluginJSON), 0644)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Format(time.RFC3339)
	_, err = db.Exec(`
		INSERT INTO extension_instances (id, ext_type, origin, catalog_id, runtime_id, install_path, name, publisher, trust_tier, config, status, error_msg, created_at, updated_at)
		VALUES ('ext_evil', 'plugin', 'user', 'cat_evil', 'plugin_1', ?, '', '', 0, '{}', '', '', ?, ?);
		INSERT INTO extension_catalog (id, marketplace_id) VALUES ('cat_evil', '');
		INSERT INTO plugins (id) VALUES ('plugin_1');
	`, pluginDir, now, now)
	if err != nil {
		t.Fatal(err)
	}

	err = mgr.UninstallExtension(ctx, "cat_evil")
	if err != nil {
		t.Fatal(err)
	}

	if runner.calledPath != "" {
		t.Errorf("expected hook to be rejected due to path traversal, but was called with: %s", runner.calledPath)
	}

	// Now test a valid hook
	runner.calledPath = ""
	pluginJSONValid := `{"hooks": {"uninstall": "valid_hook.sh"}}`
	err = os.MkdirAll(pluginDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(pluginJSONValid), 0644)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`
		INSERT INTO extension_instances (id, ext_type, origin, catalog_id, runtime_id, install_path, name, publisher, trust_tier, config, status, error_msg, created_at, updated_at)
		VALUES ('ext_valid', 'plugin', 'user', 'cat_valid', 'plugin_2', ?, '', '', 0, '{}', '', '', ?, ?);
		INSERT INTO extension_catalog (id, marketplace_id) VALUES ('cat_valid', '');
		INSERT INTO plugins (id) VALUES ('plugin_2');
	`, pluginDir, now, now)
	if err != nil {
		t.Fatal(err)
	}
	
	err = mgr.UninstallExtension(ctx, "cat_valid")
	if err != nil {
		t.Fatal(err)
	}

	if runner.calledPath == "" {
		t.Errorf("expected valid hook to be called, but it was skipped")
	}
}
