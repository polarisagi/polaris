package catalog

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type SkillCatalog struct {
	skillReg protocol.SkillRegistry
}

func NewSkillCatalog(skillReg protocol.SkillRegistry) *SkillCatalog {
	return &SkillCatalog{
		skillReg: skillReg,
	}
}

func (s *SkillCatalog) List(ctx context.Context, minTrust types.TrustTier) []CatalogEntry {
	if s.skillReg == nil {
		return nil
	}
	skills, err := s.skillReg.List(ctx, types.SkillFilter{})
	if err != nil {
		return nil
	}

	var result []CatalogEntry
	for _, sk := range skills {
		if sk.Trust >= minTrust {
			result = append(result, CatalogEntry{
				Name:        "skill:" + sk.Name,
				Description: "Auto-generated skill wrapper",
				Parameters:  nil, // Typically scripts might have dynamic schemas or none
				Source:      types.ToolSkill,
				TrustTier:   sk.Trust,
				SkillName:   sk.Name,
			})
		}
	}
	return result
}

func (s *SkillCatalog) Lookup(name string) (CatalogEntry, bool) {
	// 简单的扫描实现。生产环境可以在 List() 时做内存索引。
	entries := s.List(context.Background(), types.TrustUntrusted)
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

func (s *SkillCatalog) Register(entry CatalogEntry) {
	// Skills are registered via SkillRegistry, not here.
}

func (s *SkillCatalog) Unregister(name string) {
	// Not implemented
}

func (s *SkillCatalog) Invalidate() {
	// Stateless wrapper around skillReg
}

func (s *SkillCatalog) Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema {
	entries := s.List(ctx, minTrust)
	schemas := make([]types.ToolSchema, 0, len(entries))
	for _, e := range entries {
		schemas = append(schemas, types.ToolSchema{
			Name:        e.Name,
			Description: e.Description,
			Parameters:  e.Parameters,
		})
	}
	return schemas
}
