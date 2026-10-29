package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// queryByIDs 按事件 ID 直取情景事件（EpisodicQuery.IDs，回合内召回用：FTS 已按相关度选出 ID，这里只取正文）。
//
// 返回顺序 = IDs 顺序（即调用方给定的相关度序），Score 是按该序递减的占位分（len(IDs)-i，>0）。
// SessionID/ProjectID/Topics/MaxTaintLevel 过滤与扫描路径一致：ID 来自共享 FTS 索引，项目隔离与污点上限
// 必须在取正文这一步再判一次，不能信任调用方已筛过（fail-closed）。不存在/已损坏的 ID 静默跳过
// （FTS 索引不随事件删除而清理，命中陈旧 ID 是常态）。
func (em *EpisodicMem) queryByIDs(ctx context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error) {
	results := make([]types.ScoredEvent, 0, len(q.IDs))
	seen := make(map[string]struct{}, len(q.IDs))
	for i, id := range q.IDs {
		id = strings.TrimPrefix(id, "episodic:")
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		raw, err := em.store.Get(ctx, []byte("episodic:"+id))
		if err != nil || len(raw) == 0 {
			continue
		}
		var ev types.Event
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		if q.SessionID != "" && ev.TaskID != q.SessionID {
			continue
		}
		if q.ProjectID != "" && ev.EffectiveProjectID() != q.ProjectID {
			continue
		}
		if ev.TaintLevel > q.MaxTaintLevel {
			continue
		}
		if len(q.Topics) > 0 && !containsAny(string(ev.Payload), q.Topics) {
			continue
		}
		results = append(results, types.ScoredEvent{Event: &ev, Score: float64(len(q.IDs) - i)})
		if q.K > 0 && len(results) == q.K {
			break
		}
	}
	return results, nil
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
