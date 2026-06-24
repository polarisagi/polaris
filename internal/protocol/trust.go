package protocol

import (
	"github.com/polarisagi/polaris/pkg/types"
	// TrustTier 五级信任体系（ADR-0016 §2.1）。
	// 替代 SignatureValid bool，使系统能区分技能/工具来源的信任级别。
	// 级别越高权限越大；只有 Polaris 内部路径可以赋予 TrustSystem。
)

// TrustFromSignatureValid 向后兼容转换：SignatureValid bool → TrustTier。
// 用于数据库迁移（021_skill_trust_tier.sql），不在新代码中使用。
func TrustFromSignatureValid(valid bool) types.TrustTier {
	if valid {
		return types.TrustCommunity // 保守迁移：签名有效但 publisher 未验证
	}
	return types.TrustUntrusted
}
