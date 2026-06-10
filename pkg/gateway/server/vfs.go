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
	"strings"
	"time"

	"github.com/google/uuid"
)

// allowedUploadExts 文件上传扩展名白名单。
// 仅允许媒体/文档/数据类文件，拒绝可执行/脚本类型，防止 WebShell 上传。
var allowedUploadExts = map[string]struct{}{
	// 图片
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".webp": {}, ".bmp": {}, ".ico": {}, ".svg": {},
	// 视频
	".mp4": {}, ".webm": {}, ".mov": {}, ".avi": {}, ".mkv": {},
	// 音频
	".mp3": {}, ".wav": {}, ".ogg": {}, ".flac": {}, ".m4a": {},
	// 文档/数据
	".pdf": {}, ".txt": {}, ".md": {}, ".csv": {}, ".json": {}, ".yaml": {}, ".yml": {},
	".docx": {}, ".xlsx": {}, ".pptx": {}, ".zip": {}, ".tar": {}, ".gz": {},
}

// sanitizeUploadExt 过滤文件扩展名，仅放行白名单条目，其余返回 ".blob"。
// 同时确保扩展名以 "." 开头且全小写，防止大小写绕过。
func sanitizeUploadExt(ext string) string {
	if ext == "" {
		return ".blob"
	}
	// 强制小写 + 确保只有一个 "." 前缀（阻止 "..php" 类路径注入）
	lower := strings.ToLower(ext)
	if len(lower) < 2 || lower[0] != '.' || strings.Contains(lower[1:], ".") {
		return ".blob"
	}
	if _, ok := allowedUploadExts[lower]; ok {
		return lower
	}
	return ".blob"
}

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
	rawExt := filepath.Ext(header.Filename)

	// [P1修复] 直接使用客户端提供的文件扩展名有路径遍历和服务端执行风险：
	// 1. 扩展名可含 ".." 路径片段（如 "../../etc/passwd"）
	// 2. 可执行扩展名（.php/.py/.sh/.exe/.bat 等）在某些部署下可被直接执行
	// 策略：仅允许媒体/文档类白名单扩展名，其余一律归档为 .blob
	ext := sanitizeUploadExt(rawExt)

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
