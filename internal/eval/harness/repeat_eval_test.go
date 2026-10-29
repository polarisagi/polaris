package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/agent/loopguard"
	"github.com/polarisagi/polaris/internal/config"
)

// 无进展（原地打转）检测 Eval（HE-4；ADR-0020 重提条件：复现样本 + Eval）。
// 约束两件事：卡死轨迹必须在 stop 线收尾（防 token 浪费）；合法重复轨迹——编辑后重读、
// 轮询、参数不同的同名调用、真实会话——既不能被提醒也不能被收尾（不削弱能力）。

type repeatCall struct {
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Result string          `json:"result"`
}

type repeatTrajectory struct {
	ID     string       `json:"id"`
	Kind   string       `json:"kind"` // legit | stuck
	Source string       `json:"source"`
	Calls  []repeatCall `json:"calls"`
}

func loadRepeatTrajectories(t *testing.T) []repeatTrajectory {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "repeat_trajectories.json"))
	require.NoError(t, err)
	var f struct {
		Trajectories []repeatTrajectory `json:"trajectories"`
	}
	require.NoError(t, json.Unmarshal(data, &f))
	require.NotEmpty(t, f.Trajectories)
	return f.Trajectories
}

// play 返回首次 Warn / Stop 出现在第几次调用（1 起，0 = 未出现）。
func play(tr repeatTrajectory, warnAt, stopAt int) (warnIdx, stopIdx int) {
	d := loopguard.New(warnAt, stopAt)
	for i, c := range tr.Calls {
		f := d.Observe(c.Tool, c.Args, c.Result)
		if f.Level >= loopguard.Warn && warnIdx == 0 {
			warnIdx = i + 1
		}
		if f.Level == loopguard.Stop {
			return warnIdx, i + 1
		}
	}
	return warnIdx, 0
}

func TestRepeatEval_DefaultThresholds(t *testing.T) {
	th := config.DefaultThresholds().M4Kernel
	require.Positive(t, th.LoopRepeatWarnAt)
	require.Greater(t, th.LoopRepeatStopAt, th.LoopRepeatWarnAt)

	for _, tr := range loadRepeatTrajectories(t) {
		t.Run(tr.ID, func(t *testing.T) {
			warn, stop := play(tr, th.LoopRepeatWarnAt, th.LoopRepeatStopAt)
			t.Logf("calls=%d warn@%d stop@%d", len(tr.Calls), warn, stop)
			switch tr.Kind {
			case "stuck":
				require.NotZero(t, warn, "卡死轨迹必须先被提醒")
				require.NotZero(t, stop, "卡死轨迹必须被收尾")
				assert.Less(t, warn, stop)
				assert.Less(t, stop, len(tr.Calls), "收尾必须早于轨迹结束（否则 Eval 没有节省）")
			case "legit":
				// 慢启动健康检查（bash 轮询 6 次相同结果）允许被提醒，但绝不允许被收尾。
				assert.Zero(t, stop, "合法轨迹不得被收尾")
				if tr.ID != "poll_via_bash_slow_job" {
					assert.Zero(t, warn, "合法轨迹不得被提醒")
				}
			default:
				t.Fatalf("未知 kind %q", tr.Kind)
			}
		})
	}
}

// 阈值候选扫描：stop 线必须高于已知合法的最长相同结果序列（慢启动健康检查 6 次），且越低越省。
func TestRepeatEval_StopThresholdSweep(t *testing.T) {
	trs := loadRepeatTrajectories(t)
	for _, stopAt := range []int{4, 6, 8, 12, 20} {
		legitKilled, stuckSaved := 0, 0
		for _, tr := range trs {
			_, stop := play(tr, 3, stopAt)
			switch {
			case tr.Kind == "legit" && stop != 0:
				legitKilled++
			case tr.Kind == "stuck" && stop != 0:
				stuckSaved += len(tr.Calls) - stop
			}
		}
		t.Logf("stopAt=%-3d 误杀合法轨迹=%d 卡死轨迹节省调用=%d", stopAt, legitKilled, stuckSaved)
	}
	th := config.DefaultThresholds().M4Kernel
	for _, tr := range trs {
		if tr.Kind == "legit" {
			_, stop := play(tr, th.LoopRepeatWarnAt, th.LoopRepeatStopAt)
			assert.Zero(t, stop, tr.ID)
		}
	}
}
