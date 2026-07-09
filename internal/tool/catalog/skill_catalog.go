package catalog

import (
	"context"
	"strings"

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

func (s *SkillCatalog) List(ctx context.Context, minTrust types.TrustTier) []protocol.CatalogEntry {
	if s.skillReg == nil {
		return nil
	}
	skills, err := s.skillReg.List(ctx, types.SkillFilter{})
	if err != nil {
		return nil
	}

	var result []protocol.CatalogEntry
	for _, sk := range skills {
		if sk.Trust >= minTrust {
			name := strings.TrimPrefix(sk.Name, "skill:")
			result = append(result, protocol.CatalogEntry{
				Name:        name,
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

func (s *SkillCatalog) Lookup(name string) (protocol.CatalogEntry, bool) {
	// 简单的扫描实现。生产环境可以在 List() 时做内存索引。
	entries := s.List(context.Background(), types.TrustUntrusted)
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return protocol.CatalogEntry{}, false
}

func (s *SkillCatalog) Register(entry protocol.CatalogEntry) {
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
