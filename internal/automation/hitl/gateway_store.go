package hitl

import (
	"context"
	"encoding/json"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// RequestStore hitl_requests（044）的消费端契约（HE-3：接口在调用方定义）。
// 实现：internal/store/repo.SQLiteHITLRequestRepository。
// 同时满足 TrustHistory，装配根可把同一实例注入 Gateway 与 TrustScorer。
type RequestStore interface {
	// Insert 写入一行（pending 或直接终态）；同 id 覆盖。
	Insert(ctx context.Context, row repo.HITLRequestRow) error
	// Decide 仅对 pending 行条件更新，返回是否命中；未命中即已决/已超时/已对账/不存在。
	Decide(ctx context.Context, id, status, decidedBy, reason, responseJSON string) (bool, error)
	Get(ctx context.Context, id string) (repo.HITLRequestRow, error)
	ListPending(ctx context.Context) ([]repo.HITLRequestRow, error)
	TrustHistory
}

// newRequestRow 由 HITLPrompt 构造 pending 行。session_id 取自 ctx 的 CtxTaskIDKey
// （不扩 HITLPrompt 结构），无则留空。
func newRequestRow(ctx context.Context, p types.HITLPrompt) (repo.HITLRequestRow, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return repo.HITLRequestRow{}, apperr.Wrap(apperr.CodeInternal, "hitl_gateway: marshal failed", err)
	}
	sessionID, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	return repo.HITLRequestRow{
		ID:             p.ID,
		AgentID:        p.AgentID,
		SessionID:      sessionID,
		CheckpointType: p.CheckpointType,
		RiskLevel:      p.RiskLevel,
		TaintLevel:     int(p.TaintLevel),
		PromptJSON:     string(data),
		Status:         repo.HITLStatusPending,
		DeadlineNs:     p.DeadlineNs,
		CreatedAtMs:    time.Now().UnixMilli(),
	}, nil
}

// recordTerminal 为不经 waiter 流程的终态决策（信任降级放行、L3 门禁自动拒绝）直接落一行终态。
// 降级≠静默：这些路径以前不留任何记录，现在与人工审批同表可查。
func (g *GatewayImpl) recordTerminal(
	ctx context.Context, p types.HITLPrompt, resp types.HITLResponse, status, decidedBy string,
) error {
	row, err := newRequestRow(ctx, p)
	if err != nil {
		return err
	}
	row.Status = status
	row.DecidedBy = decidedBy
	row.Reason = resp.Reason
	row.DecidedAtMs = row.CreatedAtMs
	if data, mErr := json.Marshal(resp); mErr == nil {
		row.ResponseJSON = string(data)
	}
	if err := g.store.Insert(ctx, row); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "hitl_gateway: record terminal failed", err)
	}
	return nil
}
