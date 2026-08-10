// spec_header_counts_test 守护 state.yaml 内部自洽：章节头注释里写死的状态数
// 必须等于该状态机 states: 列表的实际长度。
//
// 背景（2026-08-10）：全局审核在同一份文件里查出两处「声明数量 ≠ 实际列表长度」
//   - `§par: M4 Plan-Act-Reflect 12-state machine` —— 实际 13 个（描述正文写的也是 13）
//   - `§outbox: M2 OutboxEntry 4-state machine`   —— 实际 6 个
//
// 这两条都是纯机械可检的，却只能靠人工通读发现。既有的 spec_consistency_test 只校验
// states 列表 ↔ Go 枚举，对"头注释说几个"完全不看，于是新增状态时改了列表、改了 Go
// 枚举，唯独忘了改标题——标题是 AI 按 §跳读 定位后**第一眼读到**的东西，失真的代价
// 是后续判断全部建立在错误的规模感上。按仓库既定原则（可机械化的发现必须转门控，
// 否则下轮必然重现，见 ADR-0062/0081/0091）补此门控。
//
// 刻意不覆盖 `§staging: M9 Self-Improvement 7-stage pipeline`：那里的 7 指
// happy-path 的 7 个流水线阶段（candidate_emit…full_promotion），states 列表另含
// rejected/rolled_back/dead_letter 三个终止态，10 ≠ 7 是语义正确的。故本门控只认
// `N-state machine` 句式，不认 `N-stage pipeline`。
package protocol

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// stateMachineHeaderRe 匹配 `# §<key>: ... <N>-state machine` 形式的章节头注释。
var stateMachineHeaderRe = regexp.MustCompile(`^#\s*§(\w+):.*?(\d+)-state machine`)

// yamlStatesKeyRe 匹配某个顶层 key 下的 `  states:` 行。
var yamlTopLevelKeyRe = regexp.MustCompile(`^(\w+):\s*$`)

func TestSpecStateMachineHeaderCounts(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	specPath := filepath.Join(filepath.Dir(file), "..", "..", "docs", "arch", "spec", "state.yaml")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("读取 state.yaml 失败: %v (路径=%s)", err, specPath)
	}
	lines := strings.Split(string(raw), "\n")

	declared := map[string]int{} // key → 头注释声明的状态数
	for _, l := range lines {
		if m := stateMachineHeaderRe.FindStringSubmatch(l); m != nil {
			n, convErr := strconv.Atoi(m[2])
			if convErr != nil {
				t.Fatalf("章节头 %q 的数字解析失败: %v", l, convErr)
			}
			declared[m[1]] = n
		}
	}
	// 匹配不到一条时必须 fail，而非静默通过——否则改写句式即可让本门控无声停摆
	// （ADR-0091：门控失效与门控通过在输出上不能长得一样）。
	if len(declared) == 0 {
		t.Fatal("state.yaml 中一条 `# §<key>: ... N-state machine` 章节头都没匹配到；" +
			"要么句式被改动导致本检查失效，要么状态机章节被删（请同步本测试）")
	}

	actual := countStatesPerTopLevelKey(lines)

	for key, want := range declared {
		got, ok := actual[key]
		if !ok {
			t.Errorf("章节头 §%s 声明为 %d-state machine，但 state.yaml 中找不到 `%s:` 顶层键下的 states: 列表", key, want, key)
			continue
		}
		if got != want {
			t.Errorf("§%s 章节头写 %d-state machine，%s.states 实际 %d 项 —— 改状态列表时须同步章节头",
				key, want, key, got)
		}
	}
}

// countStatesPerTopLevelKey 返回每个顶层 key 下 states: 列表的长度。
// 手工扫行而非 yaml.Unmarshal：本测试要校验的恰恰是**注释**与结构的关系，
// 注释在反序列化后就丢了，必须在文本层做。
func countStatesPerTopLevelKey(lines []string) map[string]int {
	out := map[string]int{}
	current := ""
	for i, l := range lines {
		if m := yamlTopLevelKeyRe.FindStringSubmatch(l); m != nil {
			current = m[1]
			continue
		}
		if current == "" || strings.TrimSpace(l) != "states:" {
			continue
		}
		n := 0
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "- ") {
				n++
				continue
			}
			break
		}
		out[current] = n
	}
	return out
}
