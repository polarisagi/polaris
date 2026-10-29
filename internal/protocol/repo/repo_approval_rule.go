package repo

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// ApprovalRuleRepository 持久化项目级“以后不再询问”授权规则（ADR-0117 决策五）。
// 关联表：052_approval_rules.sql（随项目删除级联清除）。
// @consumer: internal/agent（policy.Decide 的规则来源与落库）、internal/gateway/server/（规则管理 API）
// @producer: internal/store/repo/repo_approval_rule.go
type ApprovalRuleRepository interface {
	// AddRule 幂等新增：(project, kind, pattern) 已存在时返回已有行。kind 非法返回 CodeInvalidInput。
	AddRule(ctx context.Context, projectID, kind, pattern string) (types.ApprovalRuleRow, error)
	// ListRules 返回项目的全部规则（按创建时间升序）；projectID 为空返回所有项目的规则。
	ListRules(ctx context.Context, projectID string) ([]types.ApprovalRuleRow, error)
	// DeleteRule 按 ID 删除；不存在返回 CodeNotFound。
	DeleteRule(ctx context.Context, id string) error
	// TouchRule 记录一次命中（hit_count+1、last_used_at=now）。
	TouchRule(ctx context.Context, id string) error
	// CountByProject 返回各项目已记住的规则条数（供项目手风琴展示）。
	CountByProject(ctx context.Context) (map[string]int, error)
}
