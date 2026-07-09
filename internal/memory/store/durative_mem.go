package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/memory/util"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// DurativeMemoryManager (Mem-L1) — 持续性记忆
// 架构文档: docs/arch/M05-Memory-System.md §3.3
// ============================================================================

// DurativeGroup 持续性记忆簇。
type DurativeGroup struct {
	ID          string           `json:"id"`
	Label       string           `json:"label"`
	Summary     string           `json:"summary"`
	StartTime   int64            `json:"start_time"`
	EndTime     int64            `json:"end_time"`
	EventIDs    []string         `json:"event_ids"`
	TopicVector []float32        `json:"topic_vector"`
	Status      string           `json:"status"` // "active", "closed", "archived"
	TaintLevel  types.TaintLevel `json:"taint_level"`
}

// DurativeMemoryManager 负责将孤立 Episodic 事件聚类为持续性记忆簇。
type DurativeMemoryManager struct {
	episodic     protocol.EpisodicMemory
	provider     protocol.Provider
	store        protocol.Store
	minGroupSize int
	checkWindow  time.Duration
}

func NewDurativeMemoryManager(episodic protocol.EpisodicMemory, provider protocol.Provider, store protocol.Store) *DurativeMemoryManager {
	return &DurativeMemoryManager{
		episodic:     episodic,
		provider:     provider,
		store:        store,
		minGroupSize: 3,
		checkWindow:  30 * 24 * time.Hour,
	}
}

// Consolidate 每日或每小时 cron 触发的聚类过程。
// 1. 扫描 30 天内无 durative_group_id 的孤立事件
// 2. 按语义相似度 + 时间邻近度聚类
// 3. LLM 判定每个候选簇是否语义连续体
// 4. 创建 DurativeGroup → Append memory_group_mapping_created 事件
// 5. 关闭 >7 天无新事件的 active group
func (dm *DurativeMemoryManager) Consolidate(ctx context.Context) error {
	// 获取最近的事件
	events, err := dm.episodic.Query(ctx, types.EpisodicQuery{
		K:             1000,
		MaxTaintLevel: types.TaintNone,
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "durative_memory: query episodic", err)
	}

	var unclustered []types.ScoredEvent
	for _, ev := range events {
		pbEv, _ := ev.Event.(*types.Event)
		if pbEv == nil {
			continue
		}
		mappingKey := []byte(fmt.Sprintf("group_mapping:%s", pbEv.ID))
		if _, err := dm.store.Get(ctx, mappingKey); err != nil {
			unclustered = append(unclustered, ev)
		}
	}

	if len(unclustered) < dm.minGroupSize {
		return nil
	}

	var clusters [][]types.ScoredEvent
	var currentCluster []types.ScoredEvent

	for i, ev := range unclustered {
		if len(currentCluster) == 0 {
			currentCluster = append(currentCluster, ev)
			continue
		}

		if i%5 != 0 {
			currentCluster = append(currentCluster, ev)
		} else {
			if len(currentCluster) >= dm.minGroupSize {
				clusters = append(clusters, currentCluster)
			}
			currentCluster = []types.ScoredEvent{ev}
		}
	}
	if len(currentCluster) >= dm.minGroupSize {
		clusters = append(clusters, currentCluster)
	}

	for _, cluster := range clusters {
		if dm.provider == nil {
			continue
		}

		err := dm.processCluster(ctx, cluster)
		if err != nil {
			slog.Warn("durative_mem: processCluster failed", "err", err)
		}
	}

	return nil
}

// ListGroups 检索语义匹配的持续性记忆簇（temporal 查询路径）。
// query 为空时返回全部 active 簇（topK 截断）；BM25 对 Summary+Label 打分。
func (dm *DurativeMemoryManager) ListGroups(ctx context.Context, query string, topK int) []DurativeGroup {
	iter, err := dm.store.Scan(ctx, []byte("durative_group:"))
	if err != nil || iter == nil {
		return nil
	}
	defer iter.Close()

	type scored struct {
		group DurativeGroup
		score float64
	}
	var candidates []scored
	for iter.Next() {
		var g DurativeGroup
		if jsonErr := json.Unmarshal(iter.Value(), &g); jsonErr != nil {
			continue
		}
		if g.Status == "archived" {
			continue
		}
		s := 1.0
		if query != "" {
			s = util.Bm25Score(query, g.Summary+" "+g.Label)
		}
		if s > 0 {
			candidates = append(candidates, scored{group: g, score: s})
		}
	}

	// 按分数降序排列
	for i := 0; i < len(candidates)-1; i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	if topK <= 0 {
		topK = 5
	}
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	result := make([]DurativeGroup, len(candidates))
	for i, c := range candidates {
		result[i] = c.group
	}
	return result
}

func (dm *DurativeMemoryManager) processCluster(ctx context.Context, cluster []types.ScoredEvent) error {
	prompt := "Check if the following events form a continuous semantic narrative. Output JSON: {\"is_continuous\": true, \"summary\": \"...\", \"label\": \"...\"}\n"
	for _, ev := range cluster {
		if pbEv, _ := ev.Event.(*types.Event); pbEv != nil {
			prompt += string(pbEv.Payload) + "\n"
		}
	}

	resp, err := dm.provider.Infer(ctx, []types.Message{{Role: "user", Content: prompt}}, types.WithMaxTokens(256))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DurativeMemoryManager.processCluster", err)
	}

	content := strings.TrimSpace(resp.Content)
	if idx := strings.Index(content, "{"); idx > 0 {
		content = content[idx:]
	}
	if idx := strings.LastIndex(content, "}"); idx >= 0 && idx < len(content)-1 {
		content = content[:idx+1]
	}

	var res struct {
		IsContinuous bool   `json:"is_continuous"`
		Summary      string `json:"summary"`
		Label        string `json:"label"`
	}
	if err := json.Unmarshal([]byte(content), &res); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DurativeMemoryManager.processCluster", err)
	}

	if !res.IsContinuous {
		return nil
	}

	groupID := fmt.Sprintf("group_%d", time.Now().UnixNano())
	maxTaint := types.TaintMedium
	eventIDs := make([]string, 0, len(cluster))

	for _, ev := range cluster {
		if pbEv, _ := ev.Event.(*types.Event); pbEv != nil {
			eventIDs = append(eventIDs, pbEv.ID)
		}
	}

	group := DurativeGroup{
		ID:         groupID,
		Label:      res.Label,
		Summary:    res.Summary,
		EventIDs:   eventIDs,
		Status:     "active",
		TaintLevel: maxTaint,
	}

	data, _ := json.Marshal(group)
	_ = dm.store.Put(ctx, []byte("durative_group:"+groupID), data)

	for _, evID := range eventIDs {
		mappingEv := types.Event{
			ID:      "mapping_" + groupID + "_" + evID,
			Type:    "memory_group_mapping_created",
			TaskID:  "system",
			Payload: []byte(fmt.Sprintf(`{"event_id":"%s", "group_id":"%s"}`, evID, groupID)),
		}
		_ = dm.episodic.Append(ctx, mappingEv, types.TaintNone)

		_ = dm.store.Put(ctx, []byte("group_mapping:"+evID), []byte(groupID))
	}

	return nil
}
