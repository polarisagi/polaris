package sysadmin_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"
	"github.com/polarisagi/polaris/internal/security/checkpoint"
)

func TestHandleCheckpoints_ListAndRestore(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	testFile := filepath.Join(projectDir, "sample.txt")
	_ = os.WriteFile(testFile, []byte("v1"), 0o644)

	store, err := checkpoint.NewStore(dataDir, projectDir, 5)
	if err != nil {
		t.Fatal(err)
	}
	hash1, err := store.Create(t.Context(), "first commit")
	if err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(testFile, []byte("v2"), 0o644)
	_, err = store.Create(t.Context(), "second commit")
	if err != nil {
		t.Fatal(err)
	}

	h := &sysadmin.SysAdminHandler{
		DataDir:      dataDir,
		WorkspaceDir: projectDir,
	}

	// 1. 测试 HandleListCheckpoints
	listReq := httptest.NewRequest("GET", "/v1/checkpoints?project_dir="+projectDir, nil)
	listRec := httptest.NewRecorder()
	h.HandleListCheckpoints(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("HandleListCheckpoints returned code %d, body: %s", listRec.Code, listRec.Body.String())
	}

	var listResp struct {
		ProjectDir string `json:"project_dir"`
		Snapshots  []struct {
			Hash    string `json:"hash"`
			Message string `json:"message"`
		} `json:"snapshots"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if listResp.Count < 2 {
		t.Fatalf("expected at least 2 snapshots, got %d", listResp.Count)
	}

	// 2. 测试 HandleRestoreCheckpoint
	restoreBody, _ := json.Marshal(map[string]any{
		"project_dir": projectDir,
		"hash":        hash1,
	})
	restoreReq := httptest.NewRequest("POST", "/v1/checkpoints/restore", bytes.NewReader(restoreBody))
	restoreRec := httptest.NewRecorder()
	h.HandleRestoreCheckpoint(restoreRec, restoreReq)

	if restoreRec.Code != http.StatusOK {
		t.Fatalf("HandleRestoreCheckpoint returned code %d, body: %s", restoreRec.Code, restoreRec.Body.String())
	}

	// 验证文件已被恢复到 v1
	content, _ := os.ReadFile(testFile)
	if string(content) != "v1" {
		t.Errorf("expected restored content 'v1', got %q", string(content))
	}
}
