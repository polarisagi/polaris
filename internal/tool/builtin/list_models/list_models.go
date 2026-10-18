package list_models

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/modelcatalog"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type listModelsArgs struct {
	Query   string `json:"query"`
	Refresh bool   `json:"refresh"`
}

type ModelInfo struct {
	Ref                string   `json:"ref"`
	Name               string   `json:"name"`
	Provider           string   `json:"provider"`
	ProviderType       string   `json:"provider_type"`
	Pool               string   `json:"pool,omitempty"`
	ContextWindow      int      `json:"context_window,omitempty"`
	MaxOutput          int      `json:"max_output,omitempty"`
	SupportsThinking   bool     `json:"supports_thinking"`
	SupportsVision     bool     `json:"supports_vision"`
	SupportsTools      bool     `json:"supports_tools"`
	InputPricePerMTok  *float64 `json:"input_price_per_mtok,omitempty"`
	OutputPricePerMTok *float64 `json:"output_price_per_mtok,omitempty"`
	IsLocal            bool     `json:"is_local"`
	LockedByUser       bool     `json:"locked_by_user"`
	CurrentModel       bool     `json:"current_model"`
}

type provMeta struct {
	name string
	typ  string
}

// MakeListModelsFn 构造 list_models 工具执行函数。
func MakeListModelsFn(db protocol.SQLQuerier, cm *modelcatalog.CatalogManager) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args listModelsArgs
		if len(input) > 0 {
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "list_models: invalid args", err)
			}
		}

		if args.Refresh && cm != nil {
			if _, rErr := cm.RefreshAll(ctx); rErr != nil {
				slog.WarnContext(ctx, "list_models: refresh catalog failed", "err", rErr)
			}
		}

		if db == nil {
			return nil, apperr.New(apperr.CodeInternal, "list_models: database not configured")
		}

		currentModel, userLocked := extractModelStatus(ctx)

		providers, err := fetchProvidersMap(ctx, db)
		if err != nil {
			return nil, err
		}

		modelsMap := make(map[string]*ModelInfo)
		if err := fetchSeedModels(ctx, db, providers, modelsMap); err != nil {
			return nil, err
		}
		if err := mergeDiscoveredModels(ctx, db, providers, modelsMap); err != nil {
			return nil, err
		}

		list := filterAndSortModels(modelsMap, args.Query, userLocked, currentModel)
		out := map[string]any{
			"models":         list,
			"count":          len(list),
			"locked_by_user": userLocked != "",
			"current_model":  currentModel,
		}
		return json.Marshal(out)
	}
}

func extractModelStatus(ctx context.Context) (currentModel, userLocked string) {
	if val := ctx.Value(protocol.CtxModelSwitcherKey{}); val != nil {
		if switcher, ok := val.(protocol.ModelSwitcher); ok {
			return switcher.GetModelStatus()
		}
	}
	return "", ""
}

func fetchProvidersMap(ctx context.Context, db protocol.SQLQuerier) (map[string]provMeta, error) {
	pRows, err := db.QueryContext(ctx, `SELECT id, name, type FROM providers WHERE enabled = 1`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "list_models: query providers", err)
	}
	defer pRows.Close()

	providers := make(map[string]provMeta)
	for pRows.Next() {
		var id, name, typ string
		if err := pRows.Scan(&id, &name, &typ); err == nil {
			providers[id] = provMeta{name: name, typ: typ}
		}
	}
	if err := pRows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "list_models: pRows error", err)
	}
	return providers, nil
}

func fetchSeedModels(ctx context.Context, db protocol.SQLQuerier, providers map[string]provMeta, modelsMap map[string]*ModelInfo) error {
	mRows, err := db.QueryContext(ctx, `SELECT provider_id, model_id, name, role FROM provider_models WHERE enabled = 1`)
	if err != nil {
		return nil
	}
	defer mRows.Close()

	for mRows.Next() {
		var pID, mID, name, role string
		if err := mRows.Scan(&pID, &mID, &name, &role); err != nil {
			continue
		}
		pMeta, ok := providers[pID]
		if !ok {
			continue
		}
		ref := pID + "/" + mID
		displayName := name
		if displayName == "" {
			displayName = mID
		}
		isLocal := pMeta.typ == "ollama" || pMeta.typ == "llama_local"
		modelsMap[ref] = &ModelInfo{
			Ref:          ref,
			Name:         displayName,
			Provider:     pMeta.name,
			ProviderType: pMeta.typ,
			Pool:         role,
			IsLocal:      isLocal,
		}
	}
	if err := mRows.Err(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "list_models: mRows error", err)
	}
	return nil
}

func mergeDiscoveredModels(ctx context.Context, db protocol.SQLQuerier, providers map[string]provMeta, modelsMap map[string]*ModelInfo) error {
	dRows, err := db.QueryContext(ctx, `
		SELECT provider_id, model_id, display_name, context_window, max_output,
		       supports_thinking, supports_vision, supports_tools,
		       input_price_per_mtok, output_price_per_mtok
		  FROM provider_discovered_models`)
	if err != nil {
		return nil
	}
	defer dRows.Close()

	for dRows.Next() {
		var pID, mID, dispName string
		var cw, mo, st, sv, stl int
		var inPrice, outPrice *float64
		if err := dRows.Scan(&pID, &mID, &dispName, &cw, &mo, &st, &sv, &stl, &inPrice, &outPrice); err != nil {
			continue
		}
		pMeta, ok := providers[pID]
		if !ok {
			continue
		}
		ref := pID + "/" + mID
		info, exists := modelsMap[ref]
		if !exists {
			name := dispName
			if name == "" {
				name = mID
			}
			isLocal := pMeta.typ == "ollama" || pMeta.typ == "llama_local"
			info = &ModelInfo{
				Ref:          ref,
				Name:         name,
				Provider:     pMeta.name,
				ProviderType: pMeta.typ,
				IsLocal:      isLocal,
			}
			modelsMap[ref] = info
		}
		if dispName != "" {
			info.Name = dispName
		}
		info.ContextWindow = cw
		info.MaxOutput = mo
		info.SupportsThinking = st == 1
		info.SupportsVision = sv == 1
		info.SupportsTools = stl == 1
		info.InputPricePerMTok = inPrice
		info.OutputPricePerMTok = outPrice
	}
	if err := dRows.Err(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "list_models: dRows error", err)
	}
	return nil
}

func filterAndSortModels(modelsMap map[string]*ModelInfo, query, userLocked, currentModel string) []ModelInfo {
	q := strings.ToLower(strings.TrimSpace(query))
	var list []ModelInfo

	for _, m := range modelsMap {
		if q != "" {
			matched := strings.Contains(strings.ToLower(m.Ref), q) ||
				strings.Contains(strings.ToLower(m.Name), q) ||
				strings.Contains(strings.ToLower(m.Provider), q)
			if !matched {
				continue
			}
		}
		m.LockedByUser = userLocked != ""
		m.CurrentModel = (currentModel != "" && (m.Ref == currentModel || strings.HasSuffix(m.Ref, "/"+currentModel) || m.Pool == currentModel))
		list = append(list, *m)
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].Provider != list[j].Provider {
			return list[i].Provider < list[j].Provider
		}
		return list[i].Ref < list[j].Ref
	})
	return list
}
