package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// LLMInferFunc LLM 调用函数类型（依赖注入，可 mock）。
type LLMInferFunc func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error)

// SemanticCompressHandler 将 VFS 中的大型错误堆栈提炼为结构化 JSON，
// 保护 L0 工作记忆不被原始报错信息淹没。
type SemanticCompressHandler struct {
	db       protocol.SQLQuerier
	llmInfer LLMInferFunc
	vfsBase  string // VFS 文件根目录（如 ~/.polarisagi/polaris/data/vfs/）
}

// NewSemanticCompressHandler 创建语义压缩处理器。
func NewSemanticCompressHandler(db protocol.SQLQuerier, llmInfer LLMInferFunc, vfsBase string) *SemanticCompressHandler {
	return &SemanticCompressHandler{
		db:       db,
		llmInfer: llmInfer,
		vfsBase:  vfsBase,
	}
}

// Handle 实现 store.OutboxHandler 接口。
func (h *SemanticCompressHandler) Handle(ctx context.Context, record *store.OutboxRecord) error {
	var req struct {
		VfsID string `json:"vfs_id"`
	}
	if err := json.Unmarshal(record.Payload, &req); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "semantic_compress: parse payload failed", err)
	}

	var relPath string
	var size int64
	err := h.db.QueryRowContext(ctx, `
		SELECT file_path, size FROM workspace_vfs WHERE id = ?
	`, req.VfsID).Scan(&relPath, &size)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return apperr.Wrap(apperr.CodeInternal, "semantic_compress: query vfs failed", err)
	}

	fullPath := filepath.Join(h.vfsBase, relPath)
	f, err := os.Open(fullPath)
	if err != nil {
		// 文件可能已被删除
		slog.Warn("semantic_compress: failed to open vfs file", "path", fullPath)
		return nil
	}
	defer f.Close()

	data, _ := io.ReadAll(io.LimitReader(f, 8000))
	if len(data) == 0 {
		return nil
	}

	prompt := "以下是一段程序报错信息，请提取关键信息，输出严格 JSON，不要有任何解释：\n" +
		"{\n" +
		"  \"root_cause\": \"根本原因（≤100字）\",\n" +
		"  \"error_type\": \"错误类型（如 NullPointerException, SegFault, OutOfMemory 等）\",\n" +
		"  \"suggest_fix\": \"修复建议（≤100字）\",\n" +
		"  \"affected_file\": \"涉及的主要文件路径（无则为空字符串）\"\n" +
		"}\n" +
		"报错内容：\n" + string(data)

	resJSON, err := h.llmInfer(ctx, prompt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "semantic_compress: llm infer failed", err)
	}

	var parsed struct {
		RootCause    string `json:"root_cause"`
		ErrorType    string `json:"error_type"`
		SuggestFix   string `json:"suggest_fix"`
		AffectedFile string `json:"affected_file"`
	}
	if err := json.Unmarshal([]byte(resJSON), &parsed); err != nil {
		parsed.RootCause = "解析失败，见原始文件"
		parsed.ErrorType = "unknown"
		parsed.SuggestFix = ""
		parsed.AffectedFile = ""
		resBytes, _ := json.Marshal(parsed)
		resJSON = string(resBytes)
	}

	// 覆写原始文件
	newSize := int64(len(resJSON))
	if writeErr := os.WriteFile(fullPath, []byte(resJSON), 0600); writeErr != nil {
		return apperr.Wrap(apperr.CodeInternal, "semantic_compress: write vfs failed", writeErr)
	}

	// 更新数据库 meta 和 size
	jsonPatch := fmt.Sprintf(`{"semantic_compressed":true,"original_size":%d}`, size)
	_, sqlErr := h.db.ExecContext(ctx, `
		UPDATE workspace_vfs
		SET size = ?, meta = json_patch(COALESCE(meta,'{}'), ?)
		WHERE id = ?
	`, newSize, jsonPatch, req.VfsID)

	if sqlErr != nil {
		return apperr.Wrap(apperr.CodeInternal, "semantic_compress: update vfs table failed", sqlErr)
	}

	return nil
}
