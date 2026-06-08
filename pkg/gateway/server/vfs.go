package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// handleVFSUpload 处理前端通用工作区文件上传
// 对应路由：POST /v1/workspace/upload
func (s *Server) handleVFSUpload(w http.ResponseWriter, r *http.Request) {
	// 限制上传大小 (e.g., 100MB)
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20)
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file in form data", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 生成物理路径
	vfsRoot := filepath.Join(s.dataDir, "workspace")
	if err := os.MkdirAll(vfsRoot, 0755); err != nil {
		slog.Error("vfs: mkdir error", "err", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// 计算 SHA256
	h := sha256.New()
	tee := io.TeeReader(file, h)

	fileID := uuid.New().String()
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".blob"
	}

	fileName := fileID + ext
	filePath := filepath.Join(vfsRoot, fileName)

	dst, err := os.Create(filePath)
	if err != nil {
		slog.Error("vfs: create file error", "err", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	size, err := io.Copy(dst, tee)
	if err != nil {
		slog.Error("vfs: save file error", "err", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	hashStr := hex.EncodeToString(h.Sum(nil))
	_ = hashStr // can be used for deduplication later

	// VFS URI 我们返回 `workspace://{uuid}.ext`
	vfsURI := fmt.Sprintf("workspace://%s", fileName)

	// 将元数据写入 sys_vfs_references 数据库
	_, err = s.db.Exec(`
		INSERT INTO sys_vfs_references (vfs_ref, ref_count, blob_size, created_at)
		VALUES (?, 1, ?, ?)
		ON CONFLICT(vfs_ref) DO UPDATE SET ref_count = ref_count + 1
	`, vfsURI, size, time.Now().Unix())
	if err != nil {
		slog.Warn("vfs: failed to insert ref", "err", err)
	}

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"uri":       vfsURI,
		"name":      header.Filename,
		"size":      size,
		"mime_type": mimeType,
	})
}
