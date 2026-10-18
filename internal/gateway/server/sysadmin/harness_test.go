package sysadmin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"
	"github.com/polarisagi/polaris/internal/learning/harnessoverlay"
)

func TestSysAdminHandler_HarnessEndpoints(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, config.SubdirHarnessOverlay)
	mgr := harnessoverlay.New(overlayDir)
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("overlay init failed: %v", err)
	}

	// 提交版本 1
	h1, err := mgr.WriteAndCommit(context.Background(), map[string][]byte{
		"prompt_addenda/test.md": []byte("First rule."),
	}, "feat: rule 1", "")
	if err != nil {
		t.Fatalf("commit v1 failed: %v", err)
	}

	handler := &sysadmin.SysAdminHandler{
		DataDir: tmpDir,
	}

	// 1. 测试 GET /v1/harness/history
	reqList := httptest.NewRequest("GET", "/v1/harness/history?limit=10", nil)
	wList := httptest.NewRecorder()
	handler.HandleHarnessHistory(wList, reqList)

	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", wList.Code, wList.Body.String())
	}

	var histResp struct {
		Versions []struct {
			Hash    string `json:"hash"`
			Message string `json:"message"`
		} `json:"versions"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &histResp); err != nil {
		t.Fatalf("unmarshal history response failed: %v", err)
	}
	if histResp.Count < 2 || len(histResp.Versions) < 2 {
		t.Fatalf("expected at least 2 versions, got %d", histResp.Count)
	}
	if histResp.Versions[0].Hash != h1 {
		t.Errorf("expected top commit %s, got %s", h1, histResp.Versions[0].Hash)
	}

	// 提交版本 2
	_, err = mgr.WriteAndCommit(context.Background(), map[string][]byte{
		"prompt_addenda/test2.md": []byte("Second rule."),
	}, "feat: rule 2", "")
	if err != nil {
		t.Fatalf("commit v2 failed: %v", err)
	}

	// 2. 测试 POST /v1/harness/rollback
	rollbackBody, _ := json.Marshal(map[string]any{"version": h1})
	reqRollback := httptest.NewRequest("POST", "/v1/harness/rollback", bytes.NewReader(rollbackBody))
	wRollback := httptest.NewRecorder()
	handler.HandleHarnessRollback(wRollback, reqRollback)

	if wRollback.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", wRollback.Code, wRollback.Body.String())
	}

	var rollbackResp struct {
		Success    bool   `json:"success"`
		RestoredTo string `json:"restored_to"`
	}
	if err := json.Unmarshal(wRollback.Body.Bytes(), &rollbackResp); err != nil {
		t.Fatalf("unmarshal rollback response failed: %v", err)
	}
	if !rollbackResp.Success || rollbackResp.RestoredTo != h1 {
		t.Errorf("unexpected rollback response: %+v", rollbackResp)
	}
}
