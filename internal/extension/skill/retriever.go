package skill

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// CognitiveSearcher defines the L1/L3 retrieval capabilities required by HybridRetriever.
// Fulfilled by *store.SurrealDBCoreStore in production.
type CognitiveSearcher interface {
	VecKNN(query []float32, k int) ([]store.ScoredID, error)
	GraphSpreadingActivation(startIDs []string, maxDepth int, energyDecay float64, dormancyThreshold float64, fanOutLimit int) ([]store.ScoredID, error)
}

// EmbedFn is a function type for generating text embeddings.
type EmbedFn func(ctx context.Context, text string) ([]float32, error)

// HybridRetriever implements protocol.SkillSelector with a 3-tier retrieval strategy:
// L1: vecIndex (SurrealDB Vector KNN)
// L2: sigMatcher (Signature/Capabilities Matcher)
// L3: depGraph (PPR Dependency Graph traversal via GraphSpreadingActivation)
type HybridRetriever struct {
	registry  protocol.SkillRegistry
	cognitive CognitiveSearcher
	embedFn   EmbedFn
}

func NewHybridRetriever(registry protocol.SkillRegistry, cognitive CognitiveSearcher, embedFn EmbedFn) *HybridRetriever {
	return &HybridRetriever{
		registry:  registry,
		cognitive: cognitive,
		embedFn:   embedFn,
	}
}

func (hr *HybridRetriever) Select(ctx context.Context, hint types.TaskHint) ([]types.SkillMeta, error) {
	// Query terms from hint
	query := strings.TrimSpace(hint.TaskType + " " + strings.Join(hint.CapabilitiesNeeded, " "))
	if query == " " || query == "" {
		return hr.fallbackSelect(ctx, hint)
	}

	l1Results, l2Results := hr.fetchL1L2(ctx, hint, query)

	var l3Results []store.ScoredID
	if hr.cognitive != nil {
		seeds := hr.gatherSeeds(l1Results, l2Results)
		if len(seeds) > 0 {
			res, err := hr.cognitive.GraphSpreadingActivation(seeds, 2, 0.6, 0.1, 20)
			if err == nil {
				l3Results = res
			}
		}
	}

	scores := hr.fuseScores(l1Results, l2Results, l3Results)
	return hr.fetchAndScoreMeta(ctx, scores)
}

func (hr *HybridRetriever) fetchL1L2(ctx context.Context, hint types.TaskHint, query string) ([]store.ScoredID, []types.SkillMeta) {
	var l1Results []store.ScoredID
	var l2Results []types.SkillMeta
	var wg sync.WaitGroup

	if hr.embedFn != nil && hr.cognitive != nil {
		wg.Add(1)
		concurrent.SafeGo(ctx, "extension.skill_retriever", func(ctx context.Context) {
			defer wg.Done()
			if qVec, err := hr.embedFn(ctx, query); err == nil {
				res, err := hr.cognitive.VecKNN(qVec, 10)
				if err == nil {
					l1Results = res
				}
			}
		})
	}

	wg.Add(1)
	concurrent.SafeGo(ctx, "extension.skill_retriever", func(ctx context.Context) {
		defer wg.Done()
		list, _ := hr.registry.List(ctx, types.SkillFilter{
			Capabilities:      hint.CapabilitiesNeeded,
			RiskLevelMax:      "high",
			IncludeDeprecated: false,
		})
		l2Results = list
	})

	wg.Wait()
	return l1Results, l2Results
}

func (hr *HybridRetriever) gatherSeeds(l1 []store.ScoredID, l2 []types.SkillMeta) []string {
	seedMap := make(map[string]bool)
	var seeds []string
	for _, id := range l1 {
		if !seedMap[id.ID] {
			seedMap[id.ID] = true
			seeds = append(seeds, id.ID)
		}
	}
	for _, meta := range l2 {
		if !seedMap[meta.Name] {
			seedMap[meta.Name] = true
			seeds = append(seeds, meta.Name)
		}
	}
	return seeds
}

func (hr *HybridRetriever) fuseScores(l1 []store.ScoredID, l2 []types.SkillMeta, l3 []store.ScoredID) map[string]float64 {
	scores := make(map[string]float64)
	for _, r := range l1 {
		scores[r.ID] += r.Score * 0.4
	}
	for _, m := range l2 {
		scores[m.Name] += 1.0 * 0.4
	}
	for _, r := range l3 {
		scores[r.ID] += r.Score * 0.2
	}
	return scores
}

type scoredMeta struct {
	meta  types.SkillMeta
	score float64
}

func (hr *HybridRetriever) fetchAndScoreMeta(ctx context.Context, scores map[string]float64) ([]types.SkillMeta, error) {
	var merged []scoredMeta
	for id, score := range scores {
		meta, err := hr.registry.Get(ctx, id, "")
		if err == nil && !meta.Deprecated {
			// Apply heuristic penalty for latency
			if meta.Benchmarks.AvgLatencyMs > 5000 {
				score *= 0.3
			} else if meta.Benchmarks.AvgLatencyMs > 2000 {
				score *= 0.7
			}
			merged = append(merged, scoredMeta{meta: *meta, score: score})
		}
	}

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].score > merged[j].score
	})

	limit := 5
	if len(merged) < limit {
		limit = len(merged)
	}

	var final []types.SkillMeta
	for i := 0; i < limit; i++ {
		final = append(final, merged[i].meta)
	}

	return hr.hydrateSkills(final), nil
}

// hydrateSkills implements the progressive disclosure truncation based on context budget
// (name+desc -> workflow summary -> full instructions)
func (hr *HybridRetriever) hydrateSkills(skills []types.SkillMeta) []types.SkillMeta {
	for i := range skills {
		if i >= 2 && i < 4 {
			// Top 3-4: workflow summary (truncate instructions)
			if len(skills[i].Instructions) > 500 {
				skills[i].Instructions = skills[i].Instructions[:500] + "\n...[truncated workflow summary]..."
			}
		} else if i >= 4 {
			// Top 5+: name + desc only
			skills[i].Instructions = "[instructions omitted due to context budget]"
		}
	}
	return skills
}

// fallbackSelect provides the legacy heuristic selection if query is empty.
func (hr *HybridRetriever) fallbackSelect(ctx context.Context, hint types.TaskHint) ([]types.SkillMeta, error) {
	all, err := hr.registry.List(ctx, types.SkillFilter{
		Capabilities:      hint.CapabilitiesNeeded,
		RiskLevelMax:      "high",
		IncludeDeprecated: false,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "HybridRetriever.fallbackSelect", err)
	}

	sort.Slice(all, func(i, j int) bool {
		return hr.scoreHeuristic(all[i], hint) > hr.scoreHeuristic(all[j], hint)
	})

	if len(all) > 5 {
		all = all[:5]
	}
	return hr.hydrateSkills(all), nil
}

func (hr *HybridRetriever) scoreHeuristic(meta types.SkillMeta, hint types.TaskHint) float64 {
	capScore := 0.0
	for _, want := range hint.CapabilitiesNeeded {
		for _, has := range meta.Capabilities {
			if has == want {
				capScore += 1.0
			}
		}
	}
	if len(hint.CapabilitiesNeeded) > 0 {
		capScore /= float64(len(hint.CapabilitiesNeeded))
	}

	complexityScore := 1.0
	if hint.ComplexityScore > 0.8 && meta.RiskLevel == "low" {
		complexityScore = 0.3
	}

	passScore := meta.Benchmarks.PassRate
	if passScore < 0 {
		passScore = 0
	}

	latencyScore := 1.0
	if meta.Benchmarks.AvgLatencyMs > 5000 {
		latencyScore = 0.3
	} else if meta.Benchmarks.AvgLatencyMs > 2000 {
		latencyScore = 0.7
	}

	return capScore*0.4 + complexityScore*0.3 + passScore*0.2 + latencyScore*0.1
}
