package knowledge

import (
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// chunkTaintModule 是 rag_chunks 表跨 SQL 持久化边界的 TaintSource.Module 标识
// （M11-Policy-Safety.md §2.1 第三重防护"持久化边界密码学验证"，inv_M11_02）。
const chunkTaintModule = "knowledge.rag_chunks"

// sealChunkTaint 为写入 rag_chunks 的一条 chunk 计算 HMAC-SHA256 边界签名，
// 供 INSERT 语句写入新增的 taint_hmac 列。
//
// ser 为 nil 时返回空字符串——对应未装配 TaintBoundarySerializer 的调用方
// （目前仅限未注入 Vault 的单元测试构造器），此时该边界的第三重防护退化为
// 不启用，而非阻断写入；生产路径（cmd/polaris/boot_knowledge.go /
// boot_agent.go）恒定注入非 nil 实例。
func sealChunkTaint(ser *taint.TaintBoundarySerializer, id, content string, level int, source string) string {
	if ser == nil {
		return ""
	}
	ts := taint.NewTaintedString(content, taint.TaintSource{
		Module:           chunkTaintModule,
		EntityID:         id,
		OriginTaintLevel: types.TaintLevel(level),
	}, source)
	return ser.Seal(ts).HMACHex
}

// verifyChunkTaint 校验从 rag_chunks 读出的一条 chunk 的边界 HMAC 签名，
// 返回应当采信的 taint_level。
//
// ser 为 nil 时直接放行原始 level（未启用边界验证，与 sealChunkTaint 对称）。
// hmacHex 为空（历史行/未签名行）或校验失败（篡改）时，强制返回
// types.TaintHigh（fail-closed，inv_M11_02：反序列化路径不得绕过污点标记，
// 防御 SQL 直接篡改 taint_level 列的降级攻击）。
func verifyChunkTaint(ser *taint.TaintBoundarySerializer, id, content string, level int, source, hmacHex string) int {
	if ser == nil {
		return level
	}
	if hmacHex == "" {
		return int(types.TaintHigh)
	}
	env := taint.TaintEnvelope{
		Content: content,
		Level:   types.TaintLevel(level),
		Source: taint.TaintSource{
			Module:           chunkTaintModule,
			EntityID:         id,
			OriginTaintLevel: types.TaintLevel(level),
		},
		HMACHex: hmacHex,
	}
	recovered, ok := ser.Unseal(env)
	if !ok {
		return int(types.TaintHigh)
	}
	return int(recovered.Level())
}
