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

// MakeExtensionSearchFn 创建 search_extension 工具函数。
// 搜索策略：本地 extension_catalog 优先（5个内置市场同步缓存），再补充查询线上 MCP 注册表，合并去重。
// db 为 nil 时降级为纯网络搜索；client 为 nil 时降级为纯本地搜索。
func MakeExtensionSearchFn(db *sql.DB, client *marketplace.MCPMarketplaceClient) action.InProcessFn {
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

		// 1. 本地 extension_catalog（5个内置市场同步缓存）
		if db != nil {
			localResults, err := searchLocalCatalog(ctx, db, args.Query)
			if err != nil {
				slog.Warn("search_extension: local catalog search failed", "err", err)
			}
			for _, e := range localResults {
				seen[e.ID] = true
				results = append(results, e)
			}
			slog.Info("native: search_extension local hits", "count", len(localResults))
		}

		// 2. 线上 MCP 注册表补充（去重已出现的 ID）
		if client != nil {
			netResults, err := client.Search(ctx, args.Query)
			if err != nil {
				slog.Warn("search_extension: online registry search failed", "err", err)
			}
			for _, e := range netResults {
				if !seen[e.ID] {
					seen[e.ID] = true
					results = append(results, e)
				}
			}
			slog.Info("native: search_extension online hits (after dedup)", "total", len(results))
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
func MakeExtensionInstallFn(db *sql.DB, client *marketplace.MCPMarketplaceClient, installMgr *marketplace.Manager, hitlGateway protocol.HITL) action.InProcessFn {
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
		result := map[string]string{
			"status":        "success",
			"id":            args.ID,
			"installed_dir": installDir,
			"message":       "Extension installed successfully. The environment will auto-reload to expose new capabilities.",
		}
		return json.Marshal(result)
	}
}
