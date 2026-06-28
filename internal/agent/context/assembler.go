package agentctx

import (
	"context"
	"sort"
	"sync"

	"github.com/polarisagi/polaris/pkg/types"
)

type AssembleRequest struct {
	Query, SessionKey string
	MaxTokens         int
	MaxTaint          types.TaintLevel // 系统敏感=TaintNone
	IncludeKnowledge  bool
	SurpriseHint      float64 // GlobalSurpriseIndex().Current()
}

type ContextItem struct {
	Content, Source string // episodic|semantic|knowledge_rag|knowledge_graph
	Relevance       float64
	Taint           types.TaintLevel
}

type AssembledContext struct {
	Items    []ContextItem
	Taint    types.TaintLevel // = max(items.Taint)
	TokenEst int
}

type MemoryRetriever interface {
	Query(ctx context.Context, q string, maxTaint types.TaintLevel) ([]ContextItem, error)
}

type KnowledgeRetriever interface {
	Search(ctx context.Context, q string, depth int) ([]ContextItem, error)
}

type Assembler struct {
	mem  MemoryRetriever
	know KnowledgeRetriever // 可 nil
}

func NewAssembler(mem MemoryRetriever, know KnowledgeRetriever) *Assembler {
	return &Assembler{mem, know}
}

func (a *Assembler) Assemble(ctx context.Context, req AssembleRequest) (AssembledContext, error) {

	var (
		memItems  []ContextItem
		knowItems []ContextItem
		wg        sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if items, err := a.mem.Query(ctx, req.Query, req.MaxTaint); err == nil {
			memItems = items
		}
	}()

	if a.know != nil && req.IncludeKnowledge && req.SurpriseHint >= 0.3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			depth := 1
			if req.SurpriseHint > 0.6 {
				depth = 2
			}
			if items, err := a.know.Search(ctx, req.Query, depth); err == nil {
				knowItems = items
			}
		}()
	}

	wg.Wait()
	allItems := make([]ContextItem, 0, len(memItems)+len(knowItems))
	allItems = append(allItems, memItems...)
	allItems = append(allItems, knowItems...)

	// 3. RRF Fusion (k=60)
	fused := performRRF(allItems)

	// 4. MaxTaint filtering, Token Estimation, and Taint Calculation
	finalItems := make([]ContextItem, 0, len(fused))
	var totalTokens int
	maxTaint := types.TaintNone

	for _, item := range fused {
		if item.Taint > req.MaxTaint {
			continue
		}
		// Rough token estimation: len(content) / 4
		est := len(item.Content) / 4
		if est == 0 {
			est = 1
		}
		if req.MaxTokens > 0 && totalTokens+est > req.MaxTokens {
			continue
		}
		totalTokens += est
		finalItems = append(finalItems, item)
		if item.Taint > maxTaint {
			maxTaint = item.Taint
		}
	}

	return AssembledContext{
		Items:    finalItems,
		Taint:    maxTaint,
		TokenEst: totalTokens,
	}, nil
}

func performRRF(allItems []ContextItem) []ContextItem {
	scores := make(map[string]float64)
	itemMap := make(map[string]ContextItem)

	sourceLists := make(map[string][]ContextItem)
	for _, item := range allItems {
		sourceLists[item.Source] = append(sourceLists[item.Source], item)
	}

	for _, list := range sourceLists {
		sort.Slice(list, func(i, j int) bool {
			return list[i].Relevance > list[j].Relevance
		})
		for rank, item := range list {
			key := item.Content
			scores[key] += 1.0 / float64(60+rank+1)
			itemMap[key] = item
		}
	}

	fused := make([]ContextItem, 0, len(scores))
	for key := range scores {
		fused = append(fused, itemMap[key])
	}

	sort.Slice(fused, func(i, j int) bool {
		return scores[fused[i].Content] > scores[fused[j].Content]
	})

	return fused
}
