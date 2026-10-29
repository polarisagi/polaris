package knowledge

import (
	"testing"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// Test_inv_M11_02_ChunkTaintBoundary_RoundTrip 验证 sealChunkTaint/verifyChunkTaint
// 对同一条 chunk 的往返一致性：合法签名应原样放行 taint_level。
func Test_inv_M11_02_ChunkTaintBoundary_RoundTrip(t *testing.T) {
	ser := taint.NewTaintBoundarySerializer([]byte("test-key-32-bytes-aaaaaaaaaaaaaa"))

	hmacHex := sealChunkTaint(ser, "chunk1", "hello world", int(types.TaintUserReviewed), "ingestion")
	if hmacHex == "" {
		t.Fatal("sealChunkTaint returned empty HMAC for non-nil serializer")
	}

	got := verifyChunkTaint(ser, "chunk1", "hello world", int(types.TaintUserReviewed), "ingestion", hmacHex)
	if got != int(types.TaintUserReviewed) {
		t.Errorf("round-trip verify: expected TaintUserReviewed(%d), got %d", types.TaintUserReviewed, got)
	}
}

// Test_inv_M11_02_ChunkTaintBoundary_TamperedLevelUpgradesToHigh 验证 taint_level
// 列被篡改（降级攻击）时，读取路径必须 fail-closed 升级到 TaintHigh，而不是
// 信任被篡改后的低污点等级（M11-Policy-Safety.md §2.1 第三重防护）。
func Test_inv_M11_02_ChunkTaintBoundary_TamperedLevelUpgradesToHigh(t *testing.T) {
	ser := taint.NewTaintBoundarySerializer([]byte("test-key-32-bytes-aaaaaaaaaaaaaa"))

	// 以 TaintHigh 写入并封签
	hmacHex := sealChunkTaint(ser, "chunk1", "sensitive content", int(types.TaintHigh), "external_tool")

	// 模拟攻击者直接 UPDATE rag_chunks SET taint_level=0：读取时用篡改后的
	// level 重建信封，HMAC 必然不匹配。
	tamperedLevel := int(types.TaintNone)
	got := verifyChunkTaint(ser, "chunk1", "sensitive content", tamperedLevel, "external_tool", hmacHex)
	if got != int(types.TaintHigh) {
		t.Errorf("tampered taint_level must fail-closed to TaintHigh(%d), got %d", types.TaintHigh, got)
	}
}

// Test_inv_M11_02_ChunkTaintBoundary_MissingHMACUpgradesToHigh 验证历史行/未签名行
// （taint_hmac=”）在启用边界验证的读取路径下同样 fail-closed，不能被当作
// "合法但未签名"而直接放行——空 HMAC 与被剥离的 HMAC 从读取方视角不可区分。
func Test_inv_M11_02_ChunkTaintBoundary_MissingHMACUpgradesToHigh(t *testing.T) {
	ser := taint.NewTaintBoundarySerializer([]byte("test-key-32-bytes-aaaaaaaaaaaaaa"))

	got := verifyChunkTaint(ser, "chunk1", "content", int(types.TaintLow), "ingestion", "")
	if got != int(types.TaintHigh) {
		t.Errorf("empty taint_hmac must fail-closed to TaintHigh(%d), got %d", types.TaintHigh, got)
	}
}

// Test_ChunkTaintBoundary_NilSerializerPassthrough 验证未装配 TaintBoundarySerializer
// （sb.Vault 缺失等场景）时，读写两侧对称降级为不校验，不阻断正常读写。
func Test_ChunkTaintBoundary_NilSerializerPassthrough(t *testing.T) {
	if hmacHex := sealChunkTaint(nil, "chunk1", "content", int(types.TaintLow), "ingestion"); hmacHex != "" {
		t.Errorf("nil serializer: sealChunkTaint should return empty string, got %q", hmacHex)
	}
	got := verifyChunkTaint(nil, "chunk1", "content", int(types.TaintLow), "ingestion", "")
	if got != int(types.TaintLow) {
		t.Errorf("nil serializer: verifyChunkTaint should pass through original level(%d), got %d", types.TaintLow, got)
	}
}
