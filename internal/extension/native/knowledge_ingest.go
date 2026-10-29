package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// maxDocRuneLen 单篇摄入文档的最大字符数（按 rune 计量，200,000 rune 约 400KB~800KB 文本），
// 超过后必须截断以防止超大单篇文档耗尽 RAG 分块与向量化内存。
const maxDocRuneLen = 200000

// collectionNameRegex 校验 collection 合法性：仅允许 1~40 位小写字母、数字和下划线，
// 作为命名空间标签，防止特殊字符注入或空集合名。
var collectionNameRegex = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// KnowledgeIngester 是 knowledge_ingest 工具消费的知识摄入管线接口。
// 由 internal/knowledge.DefaultIngestionPipeline 结构性满足，无需直接耦合具体实现。
type KnowledgeIngester interface {
	Ingest(ctx context.Context, doc *knowledge.Document, initialTaint int) (*knowledge.DocTree, error)
}

// knowledgeIngestArgs 是 knowledge_ingest 的入参结构体。
type knowledgeIngestArgs struct {
	Collection string                   `json:"collection"`
	Docs       []knowledgeIngestDocItem `json:"docs"`
}

// knowledgeIngestDocItem 单篇待摄入文档入参定义。
type knowledgeIngestDocItem struct {
	URI         string `json:"uri"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Excerpt     string `json:"excerpt"`
	Source      string `json:"source"`
	PublishedAt string `json:"published_at"`
}

// knowledgeIngestResult 是 knowledge_ingest 批量执行的返回结果。
type knowledgeIngestResult struct {
	Ingested int                        `json:"ingested"`
	Failed   []knowledgeIngestFailedDoc `json:"failed"`
}

// knowledgeIngestFailedDoc 记录单篇摄入失败的详细信息。
type knowledgeIngestFailedDoc struct {
	URI   string `json:"uri"`
	Error string `json:"error"`
}

// MakeKnowledgeIngestFn 构造 knowledge_ingest 工具函数。
// 支持批量摄入（1~100 篇），对外部内容默认赋予 types.TaintHigh 高污点标记，
// 支持 uri/url 与 content/excerpt 容错回退，支持单篇超长安全截断与部分失败持续推进。
func MakeKnowledgeIngestFn(ing KnowledgeIngester) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if ing == nil {
			return nil, apperr.New(apperr.CodeInternal, "knowledge_ingest: ingester is not available")
		}

		var args knowledgeIngestArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "knowledge_ingest: invalid json input", err)
		}

		// 集合名称校验：确保符合命名空间规范
		if !collectionNameRegex.MatchString(args.Collection) {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("knowledge_ingest: invalid collection %q (must match ^[a-z0-9_]{1,40}$)", args.Collection))
		}

		// 批量文档数量限制校验（1..100）
		if len(args.Docs) < 1 || len(args.Docs) > 100 {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("knowledge_ingest: docs count must be between 1 and 100, got %d", len(args.Docs)))
		}

		result := knowledgeIngestResult{
			Ingested: 0,
			Failed:   make([]knowledgeIngestFailedDoc, 0),
		}

		for _, item := range args.Docs {
			doc, uri, err := buildDocument(args.Collection, item)
			if err != nil {
				result.Failed = append(result.Failed, knowledgeIngestFailedDoc{
					URI:   uri,
					Error: err.Error(),
				})
				continue
			}

			// 外部入库文档赋予 TaintHigh（高污点），隔离进入 RAG 安全边界
			if _, err := ing.Ingest(ctx, doc, int(types.TaintHigh)); err != nil {
				result.Failed = append(result.Failed, knowledgeIngestFailedDoc{
					URI:   uri,
					Error: err.Error(),
				})
			} else {
				result.Ingested++
			}
		}

		resBytes, err := json.Marshal(result)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "knowledge_ingest: marshal result", err)
		}
		return resBytes, nil
	}
}

// buildDocument 规范化单篇待摄入文档，处理 uri/url 及 content/excerpt 回退、超长截断并构造 Document 实体。
func buildDocument(collection string, item knowledgeIngestDocItem) (*knowledge.Document, string, error) {
	// 字段回退机制：uri 缺失时尝试 url；content 缺失时尝试 excerpt
	uri := strings.TrimSpace(item.URI)
	if uri == "" {
		uri = strings.TrimSpace(item.URL)
	}
	content := item.Content
	if content == "" {
		content = item.Excerpt
	}

	// 单篇缺少必要标识或正文时返回错误
	if uri == "" {
		return nil, "", apperr.New(apperr.CodeInvalidInput, "uri or url is required")
	}
	if content == "" {
		return nil, uri, apperr.New(apperr.CodeInvalidInput, "content or excerpt is required")
	}

	// 超长正文安全截断（防内存溢出与 LLM 越界）
	runes := []rune(content)
	isTruncated := false
	if len(runes) > maxDocRuneLen {
		content = string(runes[:maxDocRuneLen])
		isTruncated = true
	}

	// 构建元数据字典：记录集合命名空间、调用来源以及发布时间与截断标记
	meta := map[string]string{
		"collection": collection,
		"origin":     "knowledge_ingest",
	}
	if item.Source != "" {
		meta["source"] = item.Source
	}
	if item.PublishedAt != "" {
		meta["published_at"] = item.PublishedAt
	}
	if isTruncated {
		meta["truncated"] = "1"
	}

	raw := []byte(content)
	hashBytes := sha256.Sum256(raw)
	contentHash := hex.EncodeToString(hashBytes[:])

	sourceType := item.Source
	if sourceType == "" {
		sourceType = "web"
	}

	doc := &knowledge.Document{
		Ref: knowledge.DocumentRef{
			URI:         uri,
			Title:       item.Title,
			SourceType:  sourceType,
			ContentHash: contentHash,
			UpdatedAt:   time.Now().Unix(),
		},
		Raw:      raw,
		Metadata: meta,
	}
	return doc, uri, nil
}
