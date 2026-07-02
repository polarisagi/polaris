package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type searchExtensionArgs struct {
	Query string `json:"query"`
}

type installExtensionArgs struct {
	ID string `json:"id"`
}

// CognitiveSearcher 认知检索接口（消费方定义，实现由 SurrealDBCoreStore 提供）。
type CognitiveSearcher interface {
	// FTSSearch BM25 全文检索，返回 top-k 结果（docID + score）。
	FTSSearch(query string, k int) ([]ScoredResult, error)
	// VecKNN 向量近邻检索，query 为查询向量，k 为返回数量。
	VecKNN(query []float32, k int) ([]ScoredResult, error)
	// GraphSpreadingActivation 从起始节点蔓延激活图遍历。
	GraphSpreadingActivation(startIDs []string, maxDepth int, energyDecay, dormancyThreshold float64, fanOutLimit int) ([]ScoredResult, error)
}

// ScoredResult 检索结果条目。
type ScoredResult struct {
	ID    string // 形如 "ext_{extensionID}"
	Score float64
}

// EmbedFn 文本向量化函数（依赖注入，nil 时跳过向量检索）。
type EmbedFn func(ctx context.Context, text string) ([]float32, error)

// KnowledgeSearcher 知识库检索接口（消费方定义，防包循环）。
// 实现由 pkg/swarm/knowledge.KnowledgeBase 提供，通过 main.go 适配器注入。
// nil 时 knowledge_search 工具不注册（FeatureDeepRAG 未启用时的降级路径）。
type KnowledgeSearcher interface {
	// Search 执行三阶段 RAG 检索，返回 JSON 编码的结果段落列表。
	// query: 自然语言查询；topK: 最多返回段落数；docScope: 文档 ID 限定（空=全局）。
	SearchJSON(ctx context.Context, query string, topK int, docScope string) ([]byte, error)
}

// MakeExtensionSearchFn 创建 search_extension 工具函数。
//
// 检索优先级：
//  1. SurrealDB FTSSearch（BM25 全文，索引由 Extension Librarian 写入）
//  2. SurrealDB VecKNN（语义向量近邻，需 embedFn 不为 nil）
//  3. SurrealDB GraphSpreadingActivation（从 FTS/Vec 命中节点沿 "provides" 边扩散）
//  4. SQLite extension_catalog LIKE 查询（fallback）
//  5. 线上 MCP 注册表（最后兜底）
//
// cognitive 或 db 为 nil 时自动降级，不影响可用性。
//
//nolint:gocyclo
func MakeExtensionSearchFn(
	extRepo protocol.ExtensionRepository,
	client *marketplace.MCPMarketplaceClient,
	cognitive CognitiveSearcher,
	embedFn EmbedFn,
) sandbox.InProcessFn {
	//nolint:nestif
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args searchExtensionArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "search_extension: invalid args", err)
		}
		if strings.TrimSpace(args.Query) == "" {
			return nil, apperr.New(apperr.CodeInternal, "search_extension: query must not be empty")
		}

		slog.Info("native: search_extension invoked", "query", args.Query)

		seen := make(map[string]bool)
		var results []protocol.RegistryEntry
		var startIDs []string

		// Step 1: SurrealDB FTSSearch
		if cognitive != nil {
			ftsRes, err := cognitive.FTSSearch(args.Query, 10)
			if err != nil {
				slog.Warn("search_extension: FTSSearch failed", "err", err)
			} else {
				var ids []string
				for _, r := range ftsRes {
					if strings.HasPrefix(r.ID, "ext_") {
						extID := strings.TrimPrefix(r.ID, "ext_")
						ids = append(ids, extID)
						startIDs = append(startIDs, r.ID)
					}
				}
				if len(ids) > 0 && extRepo != nil {
					entries, err := fetchExtensionsByIDs(ctx, extRepo, ids)
					if err != nil {
						slog.Warn("search_extension: fetch FTS extensions failed", "err", err)
					} else {
						for _, e := range entries {
							if !seen[e.ID] {
								seen[e.ID] = true
								results = append(results, e)
							}
						}
					}
				}
			}
		}

		// Step 2: SurrealDB VecKNN
		if cognitive != nil && embedFn != nil {
			vec, err := embedFn(ctx, args.Query)
			if err != nil {
				slog.Warn("search_extension: embed query failed", "err", err)
			} else {
				vecRes, err := cognitive.VecKNN(vec, 10)
				if err != nil {
					slog.Warn("search_extension: VecKNN failed", "err", err)
				} else {
					var ids []string
					for _, r := range vecRes {
						if strings.HasPrefix(r.ID, "ext_") {
							extID := strings.TrimPrefix(r.ID, "ext_")
							ids = append(ids, extID)
							startIDs = append(startIDs, r.ID)
						}
					}
					if len(ids) > 0 && extRepo != nil {
						entries, err := fetchExtensionsByIDs(ctx, extRepo, ids)
						if err != nil {
							slog.Warn("search_extension: fetch VecKNN extensions failed", "err", err)
						} else {
							for _, e := range entries {
								if !seen[e.ID] {
									seen[e.ID] = true
									results = append(results, e)
								}
							}
						}
					}
				}
			}
		}

		// Step 3: SurrealDB GraphSpreadingActivation
		if cognitive != nil && len(startIDs) > 0 {
			graphRes, err := cognitive.GraphSpreadingActivation(startIDs, 2, 0.7, 0.1, 5)
			if err != nil {
				slog.Warn("search_extension: GraphSpreadingActivation failed", "err", err)
			} else {
				var ids []string
				for _, r := range graphRes {
					if strings.HasPrefix(r.ID, "ext_") {
						extID := strings.TrimPrefix(r.ID, "ext_")
						ids = append(ids, extID)
					}
				}
				if len(ids) > 0 && extRepo != nil {
					entries, err := fetchExtensionsByIDs(ctx, extRepo, ids)
					if err != nil {
						slog.Warn("search_extension: fetch Graph extensions failed", "err", err)
					} else {
						for _, e := range entries {
							if !seen[e.ID] {
								seen[e.ID] = true
								results = append(results, e)
							}
						}
					}
				}
			}
		}

		// Step 4: SQLite LIKE fallback
		if len(results) < 3 && extRepo != nil {
			localResults, err := searchLocalCatalog(ctx, extRepo, args.Query)
			if err != nil {
				slog.Warn("search_extension: local catalog search failed", "err", err)
			} else {
				for _, e := range localResults {
					if !seen[e.ID] {
						seen[e.ID] = true
						results = append(results, e)
					}
				}
			}
		}

		// Step 5: 线上 MCP 注册表兜底
		if len(results) == 0 && client != nil {
			netResults, err := client.Search(ctx, args.Query)
			if err != nil {
				slog.Warn("search_extension: online registry search failed", "err", err)
			} else {
				for _, e := range netResults {
					if !seen[e.ID] {
						seen[e.ID] = true
						results = append(results, e)
					}
				}
			}
		}

		if len(results) == 0 && extRepo == nil && client == nil {
			return nil, apperr.New(apperr.CodeInternal, "search_extension: no search backend available")
		}

		data, err := json.Marshal(results)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "search_extension: encode results failed", err)
		}
		return data, nil
	}
}

// searchLocalCatalog 在 extension_catalog 中做关键词子串匹配（name / description）。
func searchLocalCatalog(ctx context.Context, extRepo protocol.ExtensionRepository, query string) ([]protocol.RegistryEntry, error) {
	rows, err := extRepo.SearchCatalog(ctx, query, 50)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "searchLocalCatalog", err)
	}

	results := make([]protocol.RegistryEntry, 0, len(rows))
	for _, row := range rows {
		var e protocol.RegistryEntry
		if err := json.Unmarshal([]byte(row.Payload), &e); err != nil {
			continue
		}
		results = append(results, e)
	}
	return results, nil
}

// fetchExtensionsByIDs 根据 extensionID 列表从 extension_instances 批量查询元数据。
func fetchExtensionsByIDs(ctx context.Context, extRepo protocol.ExtensionRepository, ids []string) ([]protocol.RegistryEntry, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Wait, ExtRepo DOES NOT have ListInstancesByIDs, but we can ListCatalogByIDs,
	// Wait! original fetchExtensionsByIDs queries extension_instances!
	// Ah! Is there a ListInstancesByIDs? No. I can ListInstances and filter, or...
	// Oh, I will just list all instances and filter.
	allInsts, err := extRepo.ListInstances(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "fetchExtensionsByIDs: list instances failed", err)
	}

	idMap := make(map[string]bool)
	for _, id := range ids {
		idMap[id] = true
	}

	results := make([]protocol.RegistryEntry, 0, len(ids))
	for _, row := range allInsts {
		if !idMap[row.ID] {
			continue
		}
		var desc string
		if row.Config != "" {
			var config map[string]any
			if err := json.Unmarshal([]byte(row.Config), &config); err == nil {
				if d, ok := config["description"].(string); ok {
					desc = d
				}
			}
		}
		results = append(results, protocol.RegistryEntry{
			ID:          row.ID,
			Name:        row.Name,
			Description: desc,
			Type:        row.ExtType,
		})
	}
	return results, nil
}

func findRegistryTarget(ctx context.Context, id string, extRepo protocol.ExtensionRepository, client *marketplace.MCPMarketplaceClient) *protocol.RegistryEntry {
	if extRepo != nil {
		entry, err := extRepo.GetCatalogEntry(ctx, id)
		if err == nil && entry != nil {
			var e protocol.RegistryEntry
			if err := json.Unmarshal([]byte(entry.Payload), &e); err == nil {
				return &e
			}
		}
	}
	if client != nil {
		results, err := client.Search(ctx, id)
		if err == nil {
			for i := range results {
				if results[i].ID == id {
					return &results[i]
				}
			}
		}
	}
	return nil
}

// MakeExtensionInstallFn creates an InProcessFn for installing official extensions.
func MakeExtensionInstallFn(extRepo protocol.ExtensionRepository, client *marketplace.MCPMarketplaceClient, installMgr *marketplace.Manager, hitlGateway protocol.HITL, outboxWriter protocol.OutboxWriter) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args installExtensionArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "install_extension: invalid args", err)
		}

		slog.Info("native: install_extension invoked", "id", args.ID)

		target := findRegistryTarget(ctx, args.ID, extRepo, client)
		if target == nil {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("install_extension: exact package %q not found", args.ID))
		}

		// Security Gate check before installing
		if installMgr == nil {
			return nil, apperr.New(apperr.CodeInternal, "install_extension: security manager not initialized, refusing to install (fail-closed)")
		}

		installReq := marketplace.InstallRequest{
			Principal:   "agent",
			ExtensionID: "ext_" + target.ID, // arbitrary temporary ID
			ExtType:     target.Type,
			TrustTier:   target.TrustTier,
			Publisher:   target.Publisher,
			HasHooks:    false,
			Target:      target, // 新增：Catalog 查找结果，installer 用它定位下载包
		}
		if err := installMgr.InstallExtension(ctx, installReq); err != nil {
			if errors.Is(err, marketplace.ErrRequiresApproval) && hitlGateway != nil {
				_, _ = hitlGateway.Prompt(ctx, types.HITLPrompt{
					ID:             installReq.ExtensionID,
					CheckpointType: "security_review",
					PromptText:     "Agent requests to install extension: " + target.Name,
					Options: []types.HITLOption{
						{Key: "approve", Label: "Approve"},
						{Key: "deny", Label: "Deny"},
					},
				})
				return json.Marshal(map[string]string{
					"status":  "pending_approval",
					"message": "Installation suspended pending user approval. Please wait for user response.",
				})
			}
			return nil, apperr.Wrap(apperr.CodeForbidden, "install_extension: blocked by policy", err)
		}

		slog.Info("native: installed extension successfully via marketplace manager", "id", args.ID)

		// 投递 Outbox 任务给 ExtensionLibrarian 异步处理
		if outboxWriter != nil {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicExtensionLibrarian, "index_extension", map[string]string{"extension_id": args.ID}, "index_ext_"+args.ID)
			ev.Scope = "extension:" + args.ID
			_ = outboxWriter.Write(ctx, ev)
		}

		result := map[string]string{
			"status":  "success",
			"id":      args.ID,
			"message": "Extension installed successfully. The environment will auto-reload to expose new capabilities.",
		}
		return json.Marshal(result)
	}
}
