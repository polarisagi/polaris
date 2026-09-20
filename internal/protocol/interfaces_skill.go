package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

type

// SkillExecutor 执行 TypeScript/Python 技能脚本。
// 脚本由 M7 负责沙箱执行（M7 是沙箱的 CANONICAL SOURCE）。
SkillExecutor interface {
	ExecuteSkill(ctx context.Context, skillID string, input []byte) ([]byte, error)
	ValidateSkill(scriptBytes []byte) error
}

type

// SkillRegistry 是技能注册表。
// 未签名 skill 不可加载（cosign 验证失败 → signature_valid=false，Registry 拒绝返回）。
SkillRegistry interface {
	Register(ctx context.Context, meta types.SkillMeta) error
	Get(ctx context.Context, name, version string) (*types.SkillMeta, error)
	List(ctx context.Context, filter types.SkillFilter) ([]types.SkillMeta, error)
	Deprecate(ctx context.Context, name, version string, reason string) error
}

type

// SkillSelector — 启发式 + 向量 + 排序公式。不调 LLM。
//
// Deprecated: SkillSelector 已被 M13-bis CompositeCatalog 懒加载 + search_tools 元工具替代。
// 保留接口定义供未来向量增强 search_tools 时参考，不在生产中使用。
// 2026-09-20 标注废弃。
SkillSelector interface {
	Select(ctx context.Context, hint types.TaskHint) ([]types.SkillMeta, error)
}
