package skill

import "testing"

// TestStaticAnalyzer_MultiLanguageCoverage 验证 2026-07-12 unwired-code-audit
// 补齐的 Python/JS-TS 模式覆盖：StaticAnalyzer 原本只有 Go 语法模式，对
// SkillInstaller 实际安装的 TS/JS 技能和 LogicCollapse 生成的 Python 技能
// 形同虚设。
func TestStaticAnalyzer_MultiLanguageCoverage(t *testing.T) {
	cases := []struct {
		name string
		code string
		want bool // 期望 Passed
	}{
		{"go_os_exec", `import "os/exec"`, false},
		{"python_subprocess_import", "import subprocess\nsubprocess.run(['ls'])", false},
		{"python_os_system", "os.system('rm -rf /')", false},
		{"python_eval", "eval(user_input)", false},
		{"js_child_process_require", "const cp = require('child_process')", false},
		{"js_new_function", "new Function('return 1')()", false},
		{"clean_js", "function add(a, b) { return a + b }", true},
		{"clean_python", "def add(a, b):\n    return a + b", true},
	}
	analyzer := &StaticAnalyzer{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ar, err := analyzer.Analyze([]byte(tc.code))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ar.Passed != tc.want {
				t.Fatalf("Passed=%v, want %v (violations=%v)", ar.Passed, tc.want, ar.Violations)
			}
		})
	}
}

// TestRiskAssessor_MultiLanguageCoverage 验证风险分级同样覆盖 Python/JS-TS 的
// 网络/文件写入/shell 执行等高危模式，而非仅 Go 语法。
func TestRiskAssessor_MultiLanguageCoverage(t *testing.T) {
	cases := []struct {
		name          string
		code          string
		wantRiskLevel int
	}{
		{"js_fetch_network", "fetch('https://evil.com')", 2},
		{"python_requests_network", "requests.get('https://evil.com')", 2},
		{"js_child_process_shell", "child_process.exec('ls')", 2},
		{"python_subprocess_shell", "subprocess.run(['ls'])", 2},
		{"js_fs_write", "fs.writeFileSync('/tmp/x', 'y')", 1},
		{"pure_computation", "function add(a, b) { return a + b }", 0},
	}
	ra := &RiskAssessor{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, _ := ra.Assess([]byte(tc.code))
			if level != tc.wantRiskLevel {
				t.Fatalf("riskLevel=%d, want %d", level, tc.wantRiskLevel)
			}
		})
	}
}
