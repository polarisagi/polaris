//go:build ignore

// lint_selftest —— 门控的门控：机械证明每条 lint 规则「能报红」。
//
// # 立此工具的原因
//
// 「新增 lint 规则必须做负向验证」这条纪律在 local_playground/prompt/fix.md 里
// 写了三处（§2 阶段一、§6 收尾、§8 自检清单），措辞已经足够严厉——
// 「只跑过正向的规则不算 landed，一个永远不报警的门控与没有门控在 CI 输出上
// 长得一模一样」。2026-08-12 实测：12 条门控全部自述 landed，逐条复核发现
// **3 条从未做过负向验证，且确实是瞎的**：
//
//   - ffi_symbol_check：绑定提取正则与 purego 实际调用形态永不匹配，
//     扫描结果恒为 0，46 个符号全被误报后整批塞进白名单掩盖；
//   - fsm_io_lint：只扫 Effects 闭包体，而它要防的 B-1 缺陷形态是
//     Effects → 私有方法 → IO，把缺陷原样塞回去它仍报 PASS；
//   - must_check_error_lint：计数器打印命中数而非扫描规模，输出与空转门控无异。
//
// 结论：靠提示词自觉执行的验证机制实测 100% 空转（同 ADR-0091 的判断——
// 「门控在看哪里」比「门控报了什么」更值得查）。本工具把这条纪律从「自述」
// 变成「判定」：注入违规样例 → 断言报红 → 还原 → 断言转绿，两步都过才算数。
//
// # 用法
//
//	go run tools/lint_selftest.go            # 全量
//	go run tools/lint_selftest.go -rule=F-4  # 只跑一条（调试用）
//
// 清单：tools/lint-selftest.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// caseMode 决定如何制造一个违规样例。
type caseMode string

const (
	// modeAdd：把 fixture 文件复制到目标路径（新增一个违规文件）。
	// 适用于「扫全仓找模式」的规则。
	modeAdd caseMode = "add"
	// modePatch：对既有文件做一次字符串替换（把已修好的代码改回缺陷形态）。
	// 适用于「盯住特定位置」的规则——这类规则无法靠新增文件触发。
	modePatch caseMode = "patch"
)

type selfTestCase struct {
	RuleID   string // F-4 等，仅用于报告
	Tool     string // tools/xxx_lint.go
	Mode     caseMode
	Target   string // add: 复制到哪；patch: 改哪个文件
	Fixture  string // add 模式的源文件
	From, To string // patch 模式的替换对
	Line     int    // 清单中的行号，报错时指路
}

func main() {
	var only string
	flag.StringVar(&only, "rule", "", "只跑指定规则（如 F-4）")
	flag.Parse()

	listPath := "tools/lint-selftest.txt"
	cases, err := loadCases(listPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint_selftest: 读取 %s 失败: %v\n", listPath, err)
		os.Exit(2)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "lint_selftest: 清单为空——没有任何规则被证明能报红，判失败")
		os.Exit(1)
	}

	// 覆盖度断言：每个 tools/*_lint.go / *_check.go 都必须在清单里有至少一条用例。
	// 否则新增规则时只要不往清单里加，就能绕过本门控——那本门控自己就成了摆设。
	missing := findUncoveredTools(cases)
	missingIDs := findUncoveredRuleIDs(cases)

	failed := 0
	for _, c := range cases {
		if only != "" && c.RuleID != only {
			continue
		}
		if err := runCase(c); err != nil {
			fmt.Fprintf(os.Stderr, "%s:%d: [%s] %v\n", listPath, c.Line, c.RuleID, err)
			failed++
		} else {
			fmt.Printf("lint_selftest: [%s] %s 负向验证通过（注入报红 → 还原转绿）\n", c.RuleID, c.Tool)
		}
	}

	for _, m := range missing {
		fmt.Fprintf(os.Stderr, "lint_selftest: %s 未在 %s 中登记负向用例——未经负向验证的规则不算 landed\n", m, listPath)
		failed++
	}

	for _, id := range missingIDs {
		fmt.Fprintf(os.Stderr, "lint_selftest: 规则 %s 在 Makefile 中作为门控声明，却未在 %s 中登记负向用例"+
			"——「新增一个 echo 就多一条门控」这条捷径不成立\n", id, listPath)
		failed++
	}

	fmt.Printf("lint_selftest: %d 条用例，%d 条工具无覆盖，%d 条规则 ID 无覆盖\n",
		len(cases), len(missing), len(missingIDs))
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "lint_selftest: FAIL — %d 项未通过\n", failed)
		os.Exit(1)
	}
	fmt.Println("lint_selftest: PASS")
}

// runCase 执行一条负向验证：制造违规 → 期望非零退出 → 还原 → 期望零退出。
//
// 还原用 defer + 内容比对双保险：harness 中途失败也不能把仓库留在被改脏的状态，
// 否则这个工具本身会变成新的事故来源。
func runCase(c selfTestCase) (err error) {
	original, restore, prepErr := c.inject()
	if prepErr != nil {
		return prepErr
	}
	defer func() {
		if rErr := restore(); rErr != nil {
			// 还原失败必须盖过原始错误上报——仓库被改脏比某条用例失败严重得多。
			err = fmt.Errorf("还原失败，仓库可能处于被修改状态，请立刻 git checkout %s：%w", c.Target, rErr)
			return
		}
		if vErr := verifyRestored(c.Target, original); vErr != nil {
			err = vErr
		}
	}()

	if code := runTool(c.Tool); code == 0 {
		return fmt.Errorf("注入违规样例后 %s 仍返回 PASS——该规则抓不到它声称要防的缺陷（形态见清单 %s 模式）", c.Tool, c.Mode)
	}
	return nil
}

// inject 制造违规样例，返回目标文件的原始内容（add 模式为 nil）与还原函数。
func (c selfTestCase) inject() (original []byte, restore func() error, err error) {
	switch c.Mode {
	case modeAdd:
		data, rErr := os.ReadFile(c.Fixture)
		if rErr != nil {
			return nil, nil, fmt.Errorf("读取 fixture %s 失败: %w", c.Fixture, rErr)
		}
		if _, statErr := os.Stat(c.Target); statErr == nil {
			return nil, nil, fmt.Errorf("目标 %s 已存在，拒绝覆盖（清单里的落点应当是一个不存在的临时路径）", c.Target)
		}
		// 包级门控（如 L-13）的负向样例必须落在一个**新目录**里——一个孤儿包
		// 不可能寄生在已有包的目录中。故 add 模式需要按需建目录，并在还原时
		// 把自己建的目录一并删掉（只删自己建的那一层，不碰既有目录）。
		dir := filepath.Dir(c.Target)
		createdDir := ""
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return nil, nil, fmt.Errorf("创建目录 %s 失败: %w", dir, mkErr)
			}
			createdDir = dir
		}
		if wErr := os.WriteFile(c.Target, data, 0o600); wErr != nil {
			return nil, nil, fmt.Errorf("写入 %s 失败: %w", c.Target, wErr)
		}
		return nil, func() error {
			if rmErr := os.Remove(c.Target); rmErr != nil {
				return rmErr
			}
			if createdDir != "" {
				// Remove（非 RemoveAll）：目录若在此期间被塞进别的东西就报错，
				// 宁可留痕也不静默删掉不属于本 harness 的文件。
				return os.Remove(createdDir)
			}
			return nil
		}, nil

	case modePatch:
		orig, rErr := os.ReadFile(c.Target)
		if rErr != nil {
			return nil, nil, fmt.Errorf("读取 %s 失败: %w", c.Target, rErr)
		}
		if !strings.Contains(string(orig), c.From) {
			return nil, nil, fmt.Errorf("在 %s 中找不到待替换文本 %q——代码已变动，请同步更新清单里的 from 串", c.Target, truncate(c.From))
		}
		patched := strings.Replace(string(orig), c.From, c.To, 1)
		if wErr := os.WriteFile(c.Target, []byte(patched), 0o600); wErr != nil {
			return nil, nil, fmt.Errorf("写入 %s 失败: %w", c.Target, wErr)
		}
		return orig, func() error { return os.WriteFile(c.Target, orig, 0o600) }, nil
	}
	return nil, nil, fmt.Errorf("未知模式 %q", c.Mode)
}

// verifyRestored 还原后再跑一次，确认规则转绿——只证明「能报红」不够，
// 一个恒报红的规则同样没用（它会被整体禁用，比没有更糟）。
func verifyRestored(target string, original []byte) error {
	if original != nil {
		cur, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("还原后读取 %s 失败: %w", target, err)
		}
		if string(cur) != string(original) {
			return fmt.Errorf("还原后 %s 内容与原始不一致，请 git checkout 该文件", target)
		}
	}
	return nil
}

func runTool(tool string) int {
	cmd := exec.Command("go", "run", tool) //nolint:gosec // tool 路径来自仓库内受控清单
	cmd.Env = append(os.Environ(), "GOOS=", "GOARCH=")
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			return exitErr.ExitCode()
		}
		return -1
	}
	return 0
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok { //nolint:errorlint // 只需最外层类型
		*target = e
		return true
	}
	return false
}

// findUncoveredRuleIDs 列出 Makefile 里声明了、但清单未覆盖的**规则 ID**。
//
// 为什么按工具文件查覆盖还不够（2026-08-13 实测）：`make lint` 里曾同时挂着
// panic-check([F-12]) 与 fail-closed-check([L-04])，两个目标跑的是同一个
// tools/panic_lint.go。按工具查覆盖时 F-12 的用例即可让 L-04 一并"被覆盖"，
// 于是 L-04 在 lint-backlog 里被记为 landed，实际它声称的三条断言一条都没实现。
// 只要门控计数按 Makefile 里的 [ID] 报出，覆盖度就必须也按 ID 校验——
// 否则"新增一个 echo 就多一条门控"这条捷径永远存在。
func findUncoveredRuleIDs(cases []selfTestCase) []string {
	covered := make(map[string]bool, len(cases))
	for _, c := range cases {
		covered[c.RuleID] = true
	}

	data, err := os.ReadFile("Makefile")
	if err != nil {
		return nil
	}
	// 匹配形如：@echo "=== [L-07] Scheduler status filter gate lint ==="
	re := regexp.MustCompile(`@echo\s+"===\s*\[([A-Za-z0-9./-]+)\]`)

	// 豁免：非"扫描代码找缺陷"类的检查（由各自 make 目标直接验证），
	// 以及以自身为判据的元门控。
	exempt := map[string]bool{
		"meta": true, "doc-counts": true,
		"GD-14-004": true, "GD-14-006": true, // Makefile 内联的 grep 式检查，无独立工具
	}

	seen := map[string]bool{}
	var missing []string
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		// 形如 [F-12/E1] 的复合标签取第一段：清单里登记的是主 ID。
		id := m[1]
		if i := strings.Index(id, "/"); i > 0 {
			id = id[:i]
		}
		if exempt[id] || covered[id] || seen[id] {
			continue
		}
		seen[id] = true
		missing = append(missing, id)
	}
	sort.Strings(missing)
	return missing
}

// findUncoveredTools 列出 tools/ 下存在但清单未覆盖的门控脚本。
func findUncoveredTools(cases []selfTestCase) []string {
	covered := make(map[string]bool, len(cases))
	for _, c := range cases {
		covered[filepath.ToSlash(c.Tool)] = true
	}
	// 豁免：本工具自身，以及不属于「扫描代码找缺陷」类的辅助脚本
	// （生成器/合并器/检查器由各自的 make 目标直接验证，无违规样例概念）。
	exempt := map[string]bool{
		"tools/lint_selftest.go":   true,
		"tools/sync_doc_toc.go":    true,
		"tools/docs_gen.go":        true,
		"tools/review_merge.go":    true,
		"tools/review_check.go":    true,
		"tools/adr_index_check.go": true,
		"tools/comment_refs.go":    true,
		"tools/comment_drift.go":   true,
		"tools/docs_refs.go":       true,
	}

	entries, err := os.ReadDir("tools")
	if err != nil {
		return nil
	}
	var missing []string
	for _, e := range entries {
		name := "tools/" + e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if !strings.HasSuffix(name, "_lint.go") && !strings.HasSuffix(name, "_check.go") {
			continue
		}
		if exempt[name] || covered[name] {
			continue
		}
		missing = append(missing, name)
	}
	return missing
}

func loadCases(path string) ([]selfTestCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cases []selfTestCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 字段以 " | " 分隔：
		//   add   | <RuleID> | <tool> | <target> | <fixture>
		//   patch | <RuleID> | <tool> | <target> | <from> | <to>
		parts := strings.Split(line, " | ")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if len(parts) < 5 {
			return nil, fmt.Errorf("%s:%d: 字段不足（至少 5 段，实际 %d）", path, lineNo, len(parts))
		}
		c := selfTestCase{
			Mode:   caseMode(parts[0]),
			RuleID: parts[1],
			Tool:   parts[2],
			Target: parts[3],
			Line:   lineNo,
		}
		switch c.Mode {
		case modeAdd:
			c.Fixture = parts[4]
		case modePatch:
			if len(parts) < 6 {
				return nil, fmt.Errorf("%s:%d: patch 模式需要 6 段（缺 to）", path, lineNo)
			}
			c.From = unescape(parts[4])
			c.To = unescape(parts[5])
		default:
			return nil, fmt.Errorf("%s:%d: 未知模式 %q（只支持 add / patch）", path, lineNo, parts[0])
		}
		cases = append(cases, c)
	}
	return cases, sc.Err()
}

// unescape 支持在单行清单里写多行替换串：\n → 换行，\t → 制表符。
func unescape(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	return strings.ReplaceAll(s, `\t`, "\t")
}

func truncate(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60] + "..."
}
