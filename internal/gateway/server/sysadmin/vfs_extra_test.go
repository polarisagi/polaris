package sysadmin

import (
	"bytes"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestHandleVFSUpload(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS sys_files (id TEXT, name TEXT, size INTEGER, content_type TEXT, location TEXT, is_deleted INTEGER, uploaded_at DATETIME)")
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	h := &SysAdminHandler{
		DataDir:    tmpDir,
		DB:         db,
		SystemRepo: repo.NewSQLiteSystemRepository(db),
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	part.Write([]byte("fake data"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/vfs/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	// Create required dir
	os.MkdirAll(tmpDir+"/vfs/uploads", 0755)

	h.HandleVFSUpload(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 upload success")
	}
}
