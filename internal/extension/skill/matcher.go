package skill

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SkillIntentMatcher implements fsm.SkillMatcher for System-1 bypass (GD-13-004).
type SkillIntentMatcher struct {
	registry protocol.SkillRegistry
	embedFn  EmbedFn

	cacheMu sync.RWMutex
	cache   map[string][]float32
}

func NewSkillIntentMatcher(registry protocol.SkillRegistry, embedFn EmbedFn) *SkillIntentMatcher {
	return &SkillIntentMatcher{
		registry: registry,
		embedFn:  embedFn,
		cache:    make(map[string][]float32),
	}
}

func (m *SkillIntentMatcher) skillTextKey(name, desc, inst string) string {
	h := sha256.Sum256([]byte(name + "\x00" + desc + "\x00" + inst))
	return fmt.Sprintf("%x", h)
}

func (m *SkillIntentMatcher) cachedSkillEmbed(ctx context.Context, name, desc, inst string) []float32 {
	key := m.skillTextKey(name, desc, inst)
	m.cacheMu.RLock()
	if v, ok := m.cache[key]; ok {
		m.cacheMu.RUnlock()
		return v
	}
	m.cacheMu.RUnlock()

	text := name + " " + desc + " " + inst
	v, err := m.embedFn(ctx, text)
	if err != nil || len(v) == 0 {
		return nil
	}

	m.cacheMu.Lock()
	if len(m.cache) >= 512 {
		for k := range m.cache {
			delete(m.cache, k)
			break
		}
	}
	m.cache[key] = v
	m.cacheMu.Unlock()
	return v
}

func (m *SkillIntentMatcher) MatchIntent(rawIntent string) (string, float64, error) {
	if m.embedFn == nil || m.registry == nil {
		return "", 0, nil
	}
	ctx := context.Background()
	queryVec, err := m.embedFn(ctx, rawIntent)
	if err != nil || len(queryVec) == 0 {
		return "", 0, err
	}

	skills, err := m.registry.List(ctx, types.SkillFilter{
		IncludeDeprecated: false,
	})
	if err != nil {
		return "", 0, apperr.Wrap(apperr.CodeInternal, "registry list", err)
	}

	bestScore := float32(-1.0)
	bestSkill := ""

	for _, s := range skills {
		doc := fmt.Sprintf("Name: %s\nCapabilities: %s\nInstructions: %s",
			s.Name,
			strings.Join(s.Capabilities, ","),
			s.Instructions,
		)
		skillVec := m.cachedSkillEmbed(ctx, s.Name, doc, s.Instructions)
		if skillVec == nil {
			continue
		}
		score := ffi.VecCosineF32(queryVec, skillVec)
		if score > bestScore {
			bestScore = score
			bestSkill = s.Name
		}
	}
	return bestSkill, float64(bestScore), nil
}
