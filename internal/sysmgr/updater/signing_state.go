package updater

import (
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// 发布签名的开通状态判定（ADR-0095 决策二）。
//
// 为什么这段逻辑在 Go 里而不是写在 release.yml 的 shell 里：
//
// 签名是**两侧协议**——流水线负责产出 .sig，客户端负责验 .sig，两侧对"现在是否
// 该有签名"的判断必须永远一致。如果流水线用 shell 里的 `[ -n "$POLARIS_RELEASE_PRIVATE_KEY" ]`
// 自己判一套，客户端用 `len(m.releaseKeys) == 0` 判另一套，两套判断迟早漂移，
// 而漂移的表现是"发出去的包客户端装不上"——最贵的一类故障。
//
// 现在两侧读的是同一个信任根（releasekeys/*.pem，经同一个 loadTrustStore 解析），
// 判定跑的是同一个函数。release.yml 通过 tools/release_signing_gate.go 调用它。

// SigningState 是"公钥是否已内嵌"×"流水线是否持有私钥"的四象限。
type SigningState string

const (
	// SigningDisabled 无公钥、无私钥：签名尚未开通。
	// 流水线跳过签名，客户端退回纯 checksum。这是本特性落地初期的正常状态。
	SigningDisabled SigningState = "disabled"

	// SigningForward 有私钥、无公钥：流水线能签但客户端还不会验。
	// 仍然签——签了不亏，且等公钥提交后历史 release 立刻变得可验证；
	// 但要告警，因为此刻的签名不产生任何实际防护。
	SigningForward SigningState = "forward"

	// SigningEnforced 公钥与私钥齐备：正常态。签名 + 客户端 fail-closed 验签。
	SigningEnforced SigningState = "enforced"

	// SigningBroken 有公钥、无私钥：**必须阻断发布**。
	//
	// 这是四象限里唯一的致命组合，也是本文件存在的首要理由：客户端一旦内嵌了
	// 公钥就转为 fail-closed（取不到 .sig 即拒装，防签名剥离），此时若流水线
	// 因为 Secret 被删/过期/换仓库而签不了名，发出去的 release 会被**每一个已
	// 升级的客户端拒绝安装**——而流水线本身全绿，没有任何人会察觉，直到用户报
	// "更新一直失败"。
	//
	// 触发场景不是假想：轮换密钥时先提交了新公钥、还没来得及更新 Secret；
	// 或 fork 仓库后 Secret 未随之配置。
	SigningBroken SigningState = "broken"
)

// ShouldSign 报告该状态下流水线是否应当执行签名。
func (s SigningState) ShouldSign() bool { return s == SigningForward || s == SigningEnforced }

// IsFatal 报告该状态是否应当中止发布。
func (s SigningState) IsFatal() bool { return s == SigningBroken }

// ResolveSigningState 由"内嵌公钥数"与"流水线是否持有私钥"判定签名状态。
//
// 取 trustedKeyCount 而非直接读信任根，是为了让四象限可被单测穷举——真实调用方
// （tools/release_signing_gate.go）传的就是 len(TrustStoreFingerprints())。
func ResolveSigningState(trustedKeyCount int, hasPrivateKey bool) SigningState {
	switch {
	case trustedKeyCount == 0 && !hasPrivateKey:
		return SigningDisabled
	case trustedKeyCount == 0 && hasPrivateKey:
		return SigningForward
	case trustedKeyCount > 0 && hasPrivateKey:
		return SigningEnforced
	default:
		return SigningBroken
	}
}

// Explain 返回该状态给流水线日志用的人话说明与处置指引。
func (s SigningState) Explain(trustedKeyCount int) string {
	switch s {
	case SigningDisabled:
		return "发布签名未开通（内嵌公钥 0 个、流水线无私钥）。本次 release 不带签名，" +
			"客户端退回纯 SHA-256 校验——校验值若取自镜像则无法抵御供应链替换。" +
			"开通流程见 internal/sysmgr/updater/releasekeys/README.md"
	case SigningForward:
		return "流水线持有私钥但仓库尚未提交任何公钥（releasekeys/*.pem 为空）。" +
			"本次会签名，但没有任何客户端会去验证它——签名此刻不产生实际防护。" +
			"把 cosign.pub 提交进 releasekeys/ 后，客户端才会转为 fail-closed 验签。"
	case SigningEnforced:
		return fmt.Sprintf("发布签名已开通：内嵌 %d 个可信公钥，流水线持有私钥。"+
			"本次将签名并对照已提交的公钥自验。", trustedKeyCount)
	case SigningBroken:
		return fmt.Sprintf("**致命**：仓库已内嵌 %d 个可信公钥，但流水线取不到 POLARIS_RELEASE_PRIVATE_KEY。\n"+
			"客户端在内嵌公钥后即转为 fail-closed（取不到 .sig 一律拒装，防签名剥离），"+
			"因此这次若发出不带签名的 release，**每一个已升级的客户端都将无法安装**，"+
			"而流水线本身不会有任何报错。\n"+
			"处置二选一：\n"+
			"  (a) 配置 POLARIS_RELEASE_PRIVATE_KEY Secret（推荐）。\n"+
			"      轮换密钥的正确顺序是【先提交新公钥进 releasekeys/，再把 Secret 换成新私钥】——\n"+
			"      新旧公钥并存期间两把私钥签的都验得过；反序（先换 Secret 后提交公钥）会让\n"+
			"      自验找不到匹配的已提交公钥而中止发布。\n"+
			"  (b) 若确要停用签名，先从 releasekeys/ 移除公钥并发一个过渡版本，"+
			"待客户端升级后再停——直接删 Secret 会把存量客户端锁死在旧版本。",
			trustedKeyCount)
	default:
		return "unknown signing state"
	}
}

// ResolveSigningStateFromTrustStore 用内嵌信任根判定状态，供流水线工具调用。
func ResolveSigningStateFromTrustStore(hasPrivateKey bool) (SigningState, int, error) {
	keys := trustStore()
	st := ResolveSigningState(len(keys), hasPrivateKey)
	if st.IsFatal() {
		return st, len(keys), apperr.New(apperr.CodeInternal, st.Explain(len(keys)))
	}
	return st, len(keys), nil
}
