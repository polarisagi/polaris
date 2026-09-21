package agentctx

// 情景记忆的项目作用域（ADR-0097 决策三修订）：感知/规划阶段直接读取 SurrealDB 共享
// FTS 与情景层时，按当前会话所属项目剔除他项目的情景事件。语义实体、扩展条目等全局层
// 命中原样放行。

import (
	"context"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ftsOverfetch 过滤前的召回放大倍数：他项目命中被剔除后仍能凑满 k 条。
const ftsOverfetch = 4

// scopeProjectID 当前会话所属项目。未解析（无项目解析器的 Agent）按默认项目处理：
// fail-closed，看不到任何带具名项目标记的情景记忆。
func scopeProjectID(sCtx *fsm.StateContext) string {
	sCtx.Mu.RLock()
	id := sCtx.ProjectID
	sCtx.Mu.RUnlock()
	if id == "" {
		return types.DefaultProjectID
	}
	return id
}

// projectScopedFTS 共享 FTS 检索 + 项目过滤（读取面 P3/P4）。
// memory 为 nil 时无法反查归属：只放行确定不是情景事件的命中是不可能的，故整体返回空
// ——宁可少一段召回，也不让他项目的对话片段进入 Prompt。
func projectScopedFTS(ctx context.Context, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher,
	query string, k int, projectID string) ([]fsm.CogResult, error) {
	if memory == nil {
		return nil, nil
	}
	hits, err := cognitive.FTSSearch(query, k*ftsOverfetch)
	if err != nil {
		return nil, err //nolint:wrapcheck // 调用方按"无结果"降级，原样透传
	}
	out := make([]fsm.CogResult, 0, k)
	for _, h := range hits {
		if owner, isEpisodic := memory.EpisodicProjectOf(ctx, h.DocID); isEpisodic && owner != projectID {
			continue
		}
		out = append(out, h)
		if len(out) == k {
			break
		}
	}
	return out, nil
}
