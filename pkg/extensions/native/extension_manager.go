package native

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/action"
	"github.com/polarisagi/polaris/pkg/extensions/marketplace"
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
	db *sql.DB,
	client *marketplace.MCPMarketplaceClient,
	cognitive CognitiveSearcher,
	embedFn EmbedFn,
) action.InProcessFn {
	//nolint:nestif
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args searchExtensionArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "search_extension: invalid args", err)
		}
		if strings.TrimSpace(args.Query) == "" {
			return nil, perrors.New(perrors.CodeInternal, "search_extension: query must not be empty")
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
				if len(ids) > 0 && db != nil {
					entries, err := fetchExtensionsByIDs(ctx, db, ids)
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
					if len(ids) > 0 && db != nil {
						entries, err := fetchExtensionsByIDs(ctx, db, ids)
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
				if len(ids) > 0 && db != nil {
					entries, err := fetchExtensionsByIDs(ctx, db, ids)
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
		if len(results) < 3 && db != nil {
			localResults, err := searchLocalCatalog(ctx, db, args.Query)
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

		if len(results) == 0 && db == nil && client == nil {
			return nil, perrors.New(perrors.CodeInternal, "search_extension: no search backend available")
		}

		data, err := json.Marshal(results)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "search_extension: encode results failed", err)
		}
		return data, nil
	}
}

// searchLocalCatalog 在 extension_catalog 中做关键词子串匹配（name / description）。
func searchLocalCatalog(ctx context.Context, db *sql.DB, query string) ([]protocol.RegistryEntry, error) {
	like := "%" + strings.ToLower(query) + "%"
	rows, err := db.QueryContext(ctx,
		`SELECT payload FROM extension_catalog
		 WHERE LOWER(name) LIKE ? OR LOWER(description) LIKE ? OR LOWER(id) LIKE ? OR LOWER(publisher) LIKE ?`,
		like, like, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []protocol.RegistryEntry
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var e protocol.RegistryEntry
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			continue
		}
		results = append(results, e)
	}
	return results, rows.Err()
}

// fetchExtensionsByIDs 根据 extensionID 列表从 extension_instances 批量查询元数据。
func fetchExtensionsByIDs(ctx context.Context, db *sql.DB, ids []string) ([]protocol.RegistryEntry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf("SELECT id, name, publisher, config FROM extension_instances WHERE id IN (%s)", strings.Join(placeholders, ","))

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "fetchExtensionsByIDs: db query failed", err)
	}
	defer rows.Close()

	var results []protocol.RegistryEntry
	for rows.Next() {
		var id, name, publisher, configStr string
		if err := rows.Scan(&id, &name, &publisher, &configStr); err != nil {
			continue
		}
		var desc string
		if configStr != "" {
			var config map[string]any
			if err := json.Unmarshal([]byte(configStr), &config); err == nil {
				if d, ok := config["description"].(string); ok {
					desc = d
				}
			}
		}
		results = append(results, protocol.RegistryEntry{
			ID:          id,
			Name:        name,
			Publisher:   publisher,
			Description: desc,
		})
	}
	return results, rows.Err()
}

func findRegistryTarget(ctx context.Context, id string, db *sql.DB, client *marketplace.MCPMarketplaceClient) *protocol.RegistryEntry {
	if db != nil {
		var payload string
		err := db.QueryRowContext(ctx, "SELECT payload FROM extension_catalog WHERE id = ?", id).Scan(&payload)
		if err == nil {
			var e protocol.RegistryEntry
			if err := json.Unmarshal([]byte(payload), &e); err == nil {
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
func MakeExtensionInstallFn(db *sql.DB, client *marketplace.MCPMarketplaceClient, installMgr *marketplace.Manager, hitlGateway protocol.HITL, outboxWriter protocol.OutboxWriter) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args installExtensionArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "install_extension: invalid args", err)
		}

		slog.Info("native: install_extension invoked", "id", args.ID)

		target := findRegistryTarget(ctx, args.ID, db, client)
		if target == nil {
			return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("install_extension: exact package %q not found", args.ID))
		}

		// Security Gate check before installing
		if installMgr != nil {
			installReq := marketplace.InstallRequest{
				Principal:   "agent",
				ExtensionID: "ext_" + target.ID, // arbitrary temporary ID
				ExtType:     target.Type,
				TrustTier:   target.TrustTier,
				Publisher:   target.Publisher,
				HasHooks:    false,
			}
			if err := installMgr.InstallExtension(ctx, installReq); err != nil {
				if errors.Is(err, marketplace.ErrRequiresApproval) && hitlGateway != nil {
					_, _ = hitlGateway.Prompt(ctx, protocol.HITLPrompt{
						ID:             installReq.ExtensionID,
						CheckpointType: "security_review",
						PromptText:     "Agent requests to install extension: " + target.Name,
						Options: []protocol.HITLOption{
							{Key: "approve", Label: "Approve"},
							{Key: "deny", Label: "Deny"},
						},
					})
					return json.Marshal(map[string]string{
						"status":  "pending_approval",
						"message": "Installation suspended pending user approval. Please wait for user response.",
					})
				}
				return nil, perrors.Wrap(perrors.CodeForbidden, "install_extension: blocked by policy", err)
			}
		}

		installDir, err := client.Install(ctx, *target)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "install_extension: install failed", err)
		}

		slog.Info("native: installed extension successfully", "id", args.ID, "dir", installDir)

		// 投递 Outbox 任务给 ExtensionLibrarian 异步处理
		if outboxWriter != nil {
			_ = outboxWriter.Write(ctx, protocol.OutboxEntry{
				TargetEngine:   "extension_librarian",
				Operation:      "index_extension",
				Scope:          "extension:" + args.ID,
				Payload:        []byte(`{"extension_id":"` + args.ID + `"}`),
				IdempotencyKey: "index_ext_" + args.ID,
			})
		}

		result := map[string]string{
			"status":        "success",
			"id":            args.ID,
			"installed_dir": installDir,
			"message":       "Extension installed successfully. The environment will auto-reload to expose new capabilities.",
		}
		return json.Marshal(result)
	}
}
