package policy

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 语料门控（ADR-0122 决策五）：testdata/cmdclass_corpus.tsv 每行
// command<TAB>cwd<TAB>want_class<TAB>want_reason。
// want_class 取 safe|unknown|dangerous|catastrophic，或 !catastrophic（易误报反例，只要求非灾难性）；
// want_reason 为 * 时不校验。占位符 $ROOT=可信根、$HOME=家目录。
func TestClassifyCommand_Corpus(t *testing.T) {
	root := resolveReal(t.TempDir())
	home := resolveReal(t.TempDir())
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join("testdata", "cmdclass_corpus.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	repl := strings.NewReplacer("$ROOT", root, "$HOME", home)
	rows := 0
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		raw := sc.Text()
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		cols := strings.Split(raw, "\t")
		if len(cols) != 4 {
			t.Fatalf("语料第 %d 行应有 4 列，实得 %d: %q", line, len(cols), raw)
		}
		rows++
		cmd, cwd := repl.Replace(cols[0]), repl.Replace(cols[1])
		wantClass, wantReason := cols[2], cols[3]
		v := ClassifyCommand(cmd, CmdEnv{Roots: []string{root}, Home: home, Cwd: cwd})
		if !corpusClassOK(v.Class, wantClass) {
			t.Errorf("第 %d 行 %q（cwd=%s）: 等级 %s(%s)，期望 %s", line, cols[0], cols[1], v.Class, v.Reason, wantClass)
			continue
		}
		if wantReason != "*" && v.Reason != wantReason {
			t.Errorf("第 %d 行 %q: 原因 %q，期望 %q", line, cols[0], v.Reason, wantReason)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if rows < 80 {
		t.Fatalf("语料行数 %d < 80", rows)
	}
}

func corpusClassOK(got CmdClass, want string) bool {
	if want == "!catastrophic" {
		return got != CmdCatastrophic
	}
	return got.String() == want
}

// 原文扫描命中只能是灾难性（硬限制询问），不得伪装成自保护 Deny：可绕过的判据不能做拒绝边界（HE-2）。
func TestClassifyCommand_RawHitNeverSelfProtected(t *testing.T) {
	env := cmdTestEnv(t)
	v := ClassifyCommand(`cat $(echo ~/.polaris/vault.key)`, env)
	if v.Class != CmdCatastrophic || v.Reason != "sensitive_fragment" {
		t.Fatalf("应由原文扫描判灾难性: %s(%s)", v.Class, v.Reason)
	}
	if v.SelfProtected || v.SensitiveReason != "" {
		t.Fatalf("原文扫描不得产出自保护/敏感路径标志: %+v", v)
	}
}
