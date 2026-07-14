package eval

import (
	"github.com/polarisagi/polaris/internal/eval/harness"

	"crypto/ed25519"
	"math"
	"testing"
)

func TestFoundingAnchor(t *testing.T) {
	t.Run("ComputeFingerprint", func(t *testing.T) {
		traces := []harness.TrajectoryTrace{
			{
				ToolCalls: []harness.ToolCallRecord{
					{Name: "cmd1"}, {Name: "cmd2"}, {Name: "cmd1"},
				},
				LLMCalls: []harness.LLMCallRecord{
					{Response: map[string]any{"content": "short"}},
					{Response: map[string]any{"content": "a bit longer response"}},
				},
			},
		}

		fp := ComputeFingerprint(traces)
		if len(fp.ToolBigramDistribution) == 0 {
			t.Errorf("expected bigrams")
		}
		if fp.ToolBigramDistribution["cmd1→cmd2"] != 0.5 {
			t.Errorf("expected 0.5, got %v", fp.ToolBigramDistribution["cmd1→cmd2"])
		}
		if fp.OutputLenP50 == 0 {
			t.Errorf("expected output len")
		}
	})

	t.Run("CompareWithAnchor", func(t *testing.T) {
		anchor := &FoundingAnchor{
			Fingerprint: BehaviorFingerprint{
				ToolBigramDistribution: map[string]float64{"a→b": 1.0},
				OutputLenP50:           100,
				RefusalRate:            0.0,
				AvgSurpriseIndex:       0.0,
			},
		}

		current := BehaviorFingerprint{
			ToolBigramDistribution: map[string]float64{"b→c": 1.0},
			OutputLenP50:           200,
			RefusalRate:            0.1,
			AvgSurpriseIndex:       0.5,
		}

		report := CompareWithAnchor(anchor, current)
		if report.ToolBigramJSD <= 0 {
			t.Errorf("expected positive JSD")
		}
		if report.OutputLenDriftPct != 1.0 {
			t.Errorf("expected 1.0 drift pct")
		}
		if report.RefusalRateDelta != 0.1 {
			t.Errorf("expected 0.1 refusal rate delta")
		}
		if len(report.TriggeredAlerts) == 0 {
			t.Errorf("expected alerts")
		}
	})

	t.Run("LoadOrCreate", func(t *testing.T) {
		dir := t.TempDir()
		pub, priv, _ := ed25519.GenerateKey(nil)

		var traces []harness.TrajectoryTrace
		for i := 0; i < 100; i++ {
			traces = append(traces, harness.TrajectoryTrace{})
		}

		anchor, created, err := LoadOrCreate(dir, priv, traces)
		if err != nil {
			t.Fatal(err)
		}
		if !created {
			t.Errorf("expected created to be true")
		}

		anchor2, created2, err := LoadOrCreate(dir, priv, traces)
		if err != nil {
			t.Fatal(err)
		}
		if created2 {
			t.Errorf("expected created to be false")
		}
		if anchor2.Version != anchor.Version {
			t.Errorf("version mismatch")
		}

		if !VerifySignature(anchor, pub) {
			t.Errorf("signature verification failed")
		}

		// 2026-07-14 回归防护：篡改锚点内容后签名必须校验失败——这是
		// cmd/polaris/boot_agent.go founding-anchor-drift-detector 的
		// verifyOrDiscard 依赖的安全属性（磁盘直改绕过签名重算时必须被检出）。
		tampered := *anchor
		tampered.Fingerprint.OutputLenP50 = anchor.Fingerprint.OutputLenP50 + 999
		if VerifySignature(&tampered, pub) {
			t.Errorf("expected verification to fail for tampered fingerprint")
		}

		// 错误公钥同样必须校验失败。
		wrongPub, _, _ := ed25519.GenerateKey(nil)
		if VerifySignature(anchor, wrongPub) {
			t.Errorf("expected verification to fail for mismatched public key")
		}
	})

	t.Run("GlobalDriftScore", func(t *testing.T) {
		monitor := NewDriftMonitor()
		monitor.SetScore(0.5)
		if math.Abs(monitor.GetScore()-0.5) > 0.001 {
			t.Errorf("expected 0.5")
		}
	})
}
