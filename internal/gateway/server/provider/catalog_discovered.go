package provider

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func (h *ProviderHandler) mergeDiscoveredCatalogModels(ctx context.Context, provMap map[string]*CatalogProvider) error {
	drows, dErr := h.DB.QueryContext(ctx,
		`SELECT p.catalog_id, dm.model_id, COALESCE(dm.display_name, dm.model_id)
		   FROM provider_discovered_models dm
		   JOIN providers p ON p.id = dm.provider_id
		  WHERE p.catalog_id != ''`)
	if dErr != nil {
		return nil
	}
	defer drows.Close()

	for drows.Next() {
		var catID, mID, dispName string
		if sErr := drows.Scan(&catID, &mID, &dispName); sErr != nil {
			continue
		}
		p, ok := provMap[catID]
		if !ok || containsCatalogModelID(p.Models, mID) {
			continue
		}
		p.Models = append(p.Models, CatalogModel{
			ID:              "disc_" + mID,
			ModelID:         mID,
			DisplayName:     dispName,
			RecommendedRole: "general",
			DisplayOrder:    len(p.Models) + 1,
		})
	}
	if err := drows.Err(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mergeDiscoveredCatalogModels: rows.Err", err)
	}
	return nil
}

func containsCatalogModelID(models []CatalogModel, modelID string) bool {
	for _, m := range models {
		if m.ModelID == modelID {
			return true
		}
	}
	return false
}

func (h *ProviderHandler) mergeDiscoveredForCatalog(ctx context.Context, catalogID string, models []catalogModelRow) ([]catalogModelRow, error) {
	drows, err := h.DB.QueryContext(ctx,
		`SELECT dm.model_id, COALESCE(dm.display_name, dm.model_id)
		   FROM provider_discovered_models dm
		   JOIN providers p ON p.id = dm.provider_id
		  WHERE p.catalog_id = ?`, catalogID)
	if err != nil {
		return models, nil
	}
	defer drows.Close()

	for drows.Next() {
		var mID, dispName string
		if sErr := drows.Scan(&mID, &dispName); sErr != nil {
			continue
		}
		if containsCatalogModelRow(models, mID) {
			continue
		}
		models = append(models, catalogModelRow{
			modelID:         mID,
			displayName:     dispName,
			recommendedRole: "general",
		})
	}
	if err := drows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "fetchCatalogModels: drows error", err)
	}
	return models, nil
}

func containsCatalogModelRow(rows []catalogModelRow, modelID string) bool {
	for _, r := range rows {
		if r.modelID == modelID {
			return true
		}
	}
	return false
}
