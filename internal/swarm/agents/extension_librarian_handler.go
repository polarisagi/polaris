package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ExtensionLibrarianHandler 在扩展安装后，将其能力索引到 SurrealDB 知识图谱。
// 使 Planner 等智能体能通过语义搜索快速定位最适合的扩展。
type ExtensionLibrarianHandler struct {
	db       protocol.SQLQuerier
	surreal  SurrealWriterInterface // 复用 agents 包已定义的接口
	llmInfer LLMInferFunc           // 复用 agents 包已定义的类型
	embedFn  EmbedFunc              // 文本向量化函数（可为 nil，nil 时跳过向量索引）
}

// EmbedFunc 文本向量化函数类型（依赖注入，nil 时跳过）
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

// NewExtensionLibrarianHandler 创建扩展图书馆员处理器
func NewExtensionLibrarianHandler(
	db protocol.SQLQuerier,
	surreal SurrealWriterInterface,
	llmInfer LLMInferFunc,
	embedFn EmbedFunc,
) *ExtensionLibrarianHandler {
	return &ExtensionLibrarianHandler{
		db:       db,
		surreal:  surreal,
		llmInfer: llmInfer,
		embedFn:  embedFn,
	}
}

// Handle 实现 store.OutboxHandler 接口
func (h *ExtensionLibrarianHandler) Handle(ctx context.Context, record *store.OutboxRecord) error {
	var req struct {
		ExtensionID string `json:"extension_id"`
	}
	if err := json.Unmarshal(record.Payload, &req); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "extension_librarian: parse payload failed", err)
	}

	name, publisher, installPath, err := h.getInstanceInfo(ctx, req.ExtensionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return apperr.Wrap(apperr.CodeInternal, "extension_librarian: query instance failed", err)
	}

	docContent := h.readDocContent(installPath, name, publisher)
	parsed, err := h.analyzeContent(ctx, docContent)
	if err != nil {
		return fmt.Errorf("ExtensionLibrarianHandler.Handle: %w", err)
	}

	return h.indexAndRelate(ctx, req.ExtensionID, parsed)
}

func (h *ExtensionLibrarianHandler) getInstanceInfo(ctx context.Context, extID string) (string, string, string, error) {
	var name, publisher, installPath, configStr string
	err := h.db.QueryRowContext(ctx, `
		SELECT name, publisher, install_path, config 
		FROM extension_instances WHERE id = ?
	`, extID).Scan(&name, &publisher, &installPath, &configStr)
	if err != nil {
		return name, publisher, installPath, fmt.Errorf("ExtensionLibrarianHandler.getInstanceInfo: %w", err)
	}
	return name, publisher, installPath, nil
}

func (h *ExtensionLibrarianHandler) readDocContent(installPath, name, publisher string) string {
	for _, fn := range []string{"README.md", "AGENTS.md", "schema.json"} {
		path := filepath.Join(installPath, fn)
		if f, err := os.Open(path); err == nil {
			bytes, _ := io.ReadAll(io.LimitReader(f, 8000))
			f.Close()
			return string(bytes)
		}
	}
	return name + " by " + publisher
}

type extAnalysis struct {
	Summary      string   `json:"summary"`
	Capabilities []string `json:"capabilities"`
	BestFor      []string `json:"best_for"`
	AvoidWhen    []string `json:"avoid_when"`
}

func (h *ExtensionLibrarianHandler) analyzeContent(ctx context.Context, docContent string) (*extAnalysis, error) {
	prompt := "请分析以下工具/扩展的文档，提取其核心能力，输出严格 JSON：\n" +
		"{\n" +
		"  \"summary\": \"一句话描述（≤50字）\",\n" +
		"  \"capabilities\": [\"能力1\", \"能力2\"],\n" +
		"  \"best_for\": [\"适合场景1\", \"场景2\"],\n" +
		"  \"avoid_when\": [\"不适合场景1\"]\n" +
		"}\n" +
		"文档内容：\n" + docContent

	descJSON, err := h.llmInfer(ctx, prompt)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "extension_librarian: llm infer failed", err)
	}

	var parsed extAnalysis
	if err := json.Unmarshal([]byte(descJSON), &parsed); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "extension_librarian: parse llm response failed", err)
	}
	return &parsed, nil
}

func (h *ExtensionLibrarianHandler) indexAndRelate(ctx context.Context, extID string, parsed *extAnalysis) error {
	docID := "ext_" + extID
	indexText := parsed.Summary + " " + strings.Join(parsed.Capabilities, " ") + " " + strings.Join(parsed.BestFor, " ")

	if err := h.surreal.FTSIndex(docID, indexText); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "extension_librarian: FTSIndex failed", err)
	}

	for _, capStr := range parsed.Capabilities {
		if err := h.surreal.GraphRelate("extension:"+extID, "provides", "cap:"+capStr, 1.0); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "extension_librarian: GraphRelate failed", err)
		}
	}

	if h.embedFn != nil {
		emb, err := h.embedFn(ctx, parsed.Summary)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "extension_librarian: embed failed", err)
		}
		if err := h.surreal.VecUpsert(docID, emb); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "extension_librarian: VecUpsert failed", err)
		}
	}

	_, err := h.db.ExecContext(ctx, `
		UPDATE extension_instances 
		SET meta = json_patch(COALESCE(meta,'{}'), '{"librarian_indexed":true}') 
		WHERE id = ?
	`, extID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "extension_librarian: update meta failed", err)
	}

	return nil
}
