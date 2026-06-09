package substrate

import (
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// Test_inv_M11_02_TaintLevelOnlyRises 验证 TaintTracker.Track 只升不降。
// inv_M11_02: output = max(inputs)，受控降级仅 Sanitizer 合法路径。
func Test_inv_M11_02_TaintLevelOnlyRises(t *testing.T) {
	tt := NewTaintTracker()

	tt.Track("id1", protocol.TaintLow)
	if tt.GetLevel("id1") != protocol.TaintLow {
		t.Fatalf("inv_M11_02: initial track TaintLow failed")
	}

	// 升级到 TaintHigh
	tt.Track("id1", protocol.TaintHigh)
	if tt.GetLevel("id1") != protocol.TaintHigh {
		t.Errorf("inv_M11_02: expected TaintHigh after upgrade, got %v", tt.GetLevel("id1"))
	}

	// 尝试降级到 TaintNone——应被拦截，仍保持 TaintHigh
	tt.Track("id1", protocol.TaintNone)
	if tt.GetLevel("id1") != protocol.TaintHigh {
		t.Errorf("inv_M11_02 VIOLATED: TaintLevel dropped from TaintHigh to %v after Track(TaintNone)", tt.GetLevel("id1"))
	}

	// 尝试降级到 TaintMedium——应被拦截
	tt.Track("id1", protocol.TaintMedium)
	if tt.GetLevel("id1") != protocol.TaintHigh {
		t.Errorf("inv_M11_02 VIOLATED: TaintLevel dropped from TaintHigh to %v after Track(TaintMedium)", tt.GetLevel("id1"))
	}
}

// Test_inv_M11_02_MaxTaintEqualsMaxInput 验证 GetMaxTaint 返回输入集合的最大值。
// inv_M11_02: PropagateTaint 语义——output = max(inputs)。
func Test_inv_M11_02_MaxTaintEqualsMaxInput(t *testing.T) {
	tt := NewTaintTracker()
	tt.Track("a", protocol.TaintLow)
	tt.Track("b", protocol.TaintUserReviewed) // TaintUserReviewed(4) 是最高值
	tt.Track("c", protocol.TaintMedium)

	got := tt.GetMaxTaint("a", "b", "c")
	if got != protocol.TaintUserReviewed {
		t.Errorf("inv_M11_02: GetMaxTaint(Low,UserReviewed,Medium) = %v, want TaintUserReviewed", got)
	}
}

// Test_inv_M11_02_TaintBoundaryHMACMismatchUpgradesToHigh 验证跨边界 HMAC 不匹配时强制升级到 TaintHigh。
// inv_M11_02 的物理断裂实现：反序列化篡改数据 fail-closed（升到最高，不降级）。
func Test_inv_M11_02_TaintBoundaryHMACMismatchUpgradesToHigh(t *testing.T) {
	key := []byte("test-hmac-key-32-bytes-padded!!!!")
	ser := NewTaintBoundarySerializer(key)

	ts := NewTaintedString("safe content", TaintSource{
		Module:           "test",
		OriginTaintLevel: protocol.TaintLow,
	}, "origin")

	env := ser.Seal(ts)

	// 篡改 content——HMAC 失效
	env.Content = "injected malicious content"

	recovered, valid := ser.Unseal(env)
	if valid {
		t.Error("inv_M11_02: tampered envelope should fail HMAC verification")
	}
	if recovered.Level() != protocol.TaintHigh {
		t.Errorf("inv_M11_02: HMAC mismatch should force TaintHigh, got %v", recovered.Level())
	}
}
