package taint

import (
	"testing"
	"testing/quick"

	"github.com/polarisagi/polaris/pkg/types"
)

// TestTaintTracker_Monotonicity 验证 TaintTracker 的单调不递减属性。
func TestTaintTracker_Monotonicity(t *testing.T) {
	err := quick.Check(func(id string, level1Int, level2Int uint8) bool {
		// Map uint8 to valid TaintLevel (0 to 4)
		level1 := types.TaintLevel(level1Int % 5)
		level2 := types.TaintLevel(level2Int % 5)

		tt := NewTaintTracker()
		tt.Track(id, level1)
		first := tt.GetLevel(id)

		tt.Track(id, level2)
		second := tt.GetLevel(id)

		// Monotonicity: second must be >= first
		return second >= first && second >= level2
	}, nil)
	if err != nil {
		t.Error(err)
	}
}

// TestTaintTracker_GetMaxTaint_Commutative 验证 GetMaxTaint 的交换律属性。
func TestTaintTracker_GetMaxTaint_Commutative(t *testing.T) {
	err := quick.Check(func(id1, id2 string, l1Int, l2Int uint8) bool {
		l1 := types.TaintLevel(l1Int % 5)
		l2 := types.TaintLevel(l2Int % 5)

		tt := NewTaintTracker()
		tt.Track(id1, l1)
		tt.Track(id2, l2)

		max1 := tt.GetMaxTaint(id1, id2)
		max2 := tt.GetMaxTaint(id2, id1)

		return max1 == max2 && max1 >= l1 && max1 >= l2
	}, nil)
	if err != nil {
		t.Error(err)
	}
}

// TestTaintTracker_GetMaxTaint_Empty 验证 GetMaxTaint 为空参数时的行为。
func TestTaintTracker_GetMaxTaint_Empty(t *testing.T) {
	tt := NewTaintTracker()
	max := tt.GetMaxTaint()
	if max != types.TaintNone {
		t.Errorf("expected GetMaxTaint() to return TaintNone, got %v", max)
	}
}

// TestSanitizeToSafe_FailureExpectations 验证 SanitizeToSafe 的失败预期属性：
//   - TaintNone / TaintLow       → 允许净化（err == nil）
//   - TaintMedium / TaintHigh    → 物理阻断，必须拒绝（err != nil），不依赖内容扫描
//   - TaintUserReviewed（级别 4） → 已人工审查，允许净化（err == nil）
func TestSanitizeToSafe_FailureExpectations(t *testing.T) {
	err := quick.Check(func(content string, levelInt uint8) bool {
		level := types.TaintLevel(levelInt % 5)
		ts := NewTaintedString(content, TaintSource{OriginTaintLevel: level}, "test")

		_, err := SanitizeToSafe(ts)

		if level > types.TaintLow && level != types.TaintUserReviewed {
			// TaintMedium / TaintHigh：净化必须失败（物理阻断）
			return err != nil
		}

		// TaintNone / TaintLow / TaintUserReviewed：净化必须成功
		return err == nil
	}, nil)
	if err != nil {
		t.Error(err)
	}
}
