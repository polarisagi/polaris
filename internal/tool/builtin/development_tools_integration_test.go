package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestRegisterDevelopmentTools_FileEditUsesExecuteToolAndPersistentStore(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "state.db")
	stateStore, err := store.OpenSQLite(storePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	reg := tool.NewInMemoryToolRegistry(newBuiltinTestEnvelope(sbx, action.GetTokenManager()), config.DefaultThresholds().M7Tool)
	reg.WithPolicy(policy.ProfilePolicy(policy.ProfileFull))
	if err := RegisterDevelopmentTools(sbx, reg, DevelopmentToolDeps{AllowedPaths: []string{root}, Store: stateStore, Registry: reg}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file_edit", "project_diagnostics", "project_preview", "code_patch_test"} {
		meta, err := reg.Lookup(name)
		schema, ok := meta.InputSchema.(map[string]any)
		if err != nil || meta.Name != name || !ok || len(schema) == 0 {
			t.Fatalf("development tool %s not registered with schema: meta=%+v err=%v", name, meta, err)
		}
	}
	if meta, err := reg.Lookup("code_patch_test"); err != nil || meta.Timeout.Minutes() != 10 {
		t.Fatalf("code_patch_test timeout=%v err=%v, want 10m", meta.Timeout, err)
	}
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "session-integration")
	ctx = context.WithValue(ctx, protocol.CtxSessionIDKey{}, "session-integration")
	ctx = protocol.WithProjectRoot(ctx, root)
	fileHash := sha256.Sum256([]byte("old"))
	previewArgs, _ := json.Marshal(map[string]any{
		"path": root, "action": "preview", "edits": []map[string]any{{
			"path": "a.txt", "content": "new", "expected_sha256": hex.EncodeToString(fileHash[:]),
		}},
	})
	previewCtx, revokePreview := withDevelopmentToolToken(t, ctx)
	preview, err := reg.ExecuteTool(previewCtx, "file_edit", previewArgs, types.TaintNone)
	revokePreview()
	if err != nil || preview == nil || !preview.Success || preview.TaintLevel != types.TaintHigh {
		t.Fatalf("file_edit preview did not use the high-taint ExecuteTool path: result=%+v err=%v", preview, err)
	}
	var transaction struct {
		TransactionID string `json:"transaction_id"`
	}
	if err := json.Unmarshal(preview.Output, &transaction); err != nil || transaction.TransactionID == "" {
		t.Fatalf("preview output missing transaction ID: %s err=%v", preview.Output, err)
	}
	applyArgs, _ := json.Marshal(map[string]string{"path": root, "action": "apply", "transaction_id": transaction.TransactionID})
	applyCtx, revokeApply := withDevelopmentToolToken(t, ctx)
	_, err = reg.ExecuteTool(applyCtx, "file_edit", applyArgs, types.TaintNone)
	revokeApply()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(got) != "new" {
		t.Fatalf("file_edit apply not observed: content=%q err=%v", got, err)
	}
	loaded, err := stateStore.Get(ctx, []byte("builtin:file_edit:v1:"+transaction.TransactionID))
	if err != nil || len(loaded) == 0 {
		t.Fatalf("journal was not persisted to Store: size=%d err=%v", len(loaded), err)
	}
}

func withDevelopmentToolToken(t *testing.T, ctx context.Context) (context.Context, func()) {
	t.Helper()
	tok, err := action.NewJITToken("development-tool-integration-test", []action.TokenOperation{{ToolName: "file_edit", MaxCalls: 1}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	callCtx := context.WithValue(ctx, protocol.CtxCapabilityTokenKey{}, tok)
	return callCtx, func() { action.GetTokenManager().Revoke(tok.Claims.TokenID) }
}
