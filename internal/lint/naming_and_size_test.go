// Package lint_test 补充两条 Phase 8 治理闭环规则：
//   - Test_inv_NoForbiddenVerbRoot：R2.2 动词词根扫描，防止 Load*/Query*/Fetch*/
//     Retrieve* 类命名重新扩散（2026-07-06 审计 + 2026-07-07 复核修复了存量 33
//     处违规，本文件是"不让它再长回来"的机械检查）。
//   - Test_inv_FileLineLimit：R7 文件行数硬上限（≤400 行）CI 门控，覆盖存量代码
//     而不只是新增 diff（2026-07-07 复核发现原 .golangci.yml 配置对存量文件
//     未生效，60 个文件早已超标却从未被拦下）。
//
// 单独成文件而非塞进已经 1200+ 行的 inv_lint_test.go：本次审计的核心结论之一就是
// "大文件应该拆而不是继续堆"，本文件的存在方式本身也应遵循这条结论。
package lint_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── inv_NoForbiddenVerbRoot ──────────────────────────────────────────────────

// forbiddenVerbRootAllowlist 收录的每一项都必须在旁边写清楚"为什么不遵循 R2.2"，
// 不接受无理由加白名单——新增条目视为放宽规则，需在 PR 描述里说明理由（R8）。
var forbiddenVerbRootAllowlist = map[string]string{
	// dlopen 动态库加载：有副作用的初始化动作，不是"读数据返回调用方"语义，
	// Get/List/FindBy 均不贴切；且 LoadLibrary 是历史悠久的跨语言惯用名
	// （Windows API 同名函数）。
	"LoadLibrary": "内置工具/引擎动态库加载(dlopen)，非数据读取语义",
	// 模型加载进运行时内存，同样是有副作用的初始化动作。
	"LoadModel": "AI 模型加载进运行时内存，非数据读取语义",
	// 复合语义：不存在则创建。镜像 Go 标准库 sync.Map.LoadOrStore 的既有命名惯例，
	// 强改 GetOrCreate 反而偏离社区共识。
	"LoadOrCreate": "镜像 sync.Map.LoadOrStore 命名惯例的 create-if-absent 语义",
	// 镜像 database/sql 标准库接口方法名（SQLQuerier 等协议接口的实现方法）。
	"QueryContext":    "镜像 database/sql 标准库接口方法名",
	"QueryRowContext": "镜像 database/sql 标准库接口方法名",
	// 组件访问器命名，与同构的 Working()/Episodic()/Semantic()/Procedural()/
	// Reflection() 一致：返回子组件本身（名词 Retriever，检索子系统名），
	// 不是执行"读取数据"动作的动词 Retrieve，故不落入 R2.2 管辖范围。
	"Retriever": "组件访问器命名(同构 Working/Episodic/Semantic/Procedural/Reflection)，返回子组件而非执行检索动作",
}

// forbiddenVerbRoots 参照 docs/specs/00-Constitution.md R2.2：
// 读单条 → Get；读多条 → List；按条件读 → FindBy<Field>；
// 禁用 Fetch/Load/Retrieve/Query 与之并存表达同一"读"语义。
var forbiddenVerbRoots = []string{"Fetch", "Retrieve", "Load", "Query"}

// Test_inv_NoForbiddenVerbRoot 扫描 internal/ 下导出函数/方法名，禁止新增
// R2.2 违禁动词词根，白名单外一律拦截。
func Test_inv_NoForbiddenVerbRoot(t *testing.T) {
	root := repoRoot(t)

	var violations []violation
	walkGoFilesUnder(t, root, "internal", nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			name := fn.Name.Name
			if !ast.IsExported(name) {
				continue // 私有方法命名不在 R2.2 强制范围内，允许更自由的内部命名
			}
			if _, ok := forbiddenVerbRootAllowlist[name]; ok {
				continue
			}
			for _, root := range forbiddenVerbRoots {
				if strings.HasPrefix(name, root) && name != root {
					pos := fset.Position(fn.Pos())
					violations = append(violations, violation{
						relPath: relPath,
						line:    pos.Line,
						detail: fmt.Sprintf(
							"func %s — 动词词根 %q 违反 R2.2 命名字典，应改用 Get<单条>/List<多条>/FindBy<Field>；"+
								"若确有必要豁免，须在 internal/lint/naming_and_size_test.go 的 forbiddenVerbRootAllowlist 中登记理由",
							name, root),
					})
					break
				}
			}
		}
	})

	for _, v := range violations {
		t.Errorf("inv_NoForbiddenVerbRoot VIOLATED: %s", v)
	}
}

// ─── inv_FileLineLimit ────────────────────────────────────────────────────────

// fileLineLimitExemptSuffixes 结构性豁免：生成代码不受手写代码的可读性上限约束。
var fileLineLimitExemptSuffixes = []string{".pb.go"}

// fileLineLimitBaselinePath 存量超标文件的冻结名单（R7 ratchet，只减不增）。
// 新文件/新增代码不得依赖此名单——名单只承接 2026-07-06 审计时已存在的历史债务，
// 每拆完一个从名单移除；新违规不得加入,必须当场拆分。
const fileLineLimitBaselinePath = "file_line_limit_baseline.json"

const fileLineLimitMax = 400

// Test_inv_FileLineLimit 验证 R7"文件行数 ≤400"对存量代码同样生效，不只是
// .golangci.yml 里对着 diff 生效的 lll 规则。
//
// 2026-07-07 新增背景：2026-07-06 审计发现 60 个文件超标，整改分支处理后仍有
// 59 个超标（见 local_playground/reports/
// code-quality-remediation-verification-20260707.md），说明存量超标只能靠人工
// 记得去修，没有机械门控会一直反复出现同样的问题。本规则用 baseline ratchet
// 模式落地：baseline 里的文件允许继续超标（历史债务，逐个偿还），baseline 之外
// 新出现的超标文件一律拦截，防止"又长出一个 2000 行的文件"。
func Test_inv_FileLineLimit(t *testing.T) {
	root := repoRoot(t)
	exempt := loadExemptFile(t, root, fileLineLimitBaselinePath)

	var violations []violation
	walkGoFilesUnder(t, root, "internal", nil, func(_ *token.FileSet, _ *ast.File, relPath string) {
		checkFileLineLimit(t, root, relPath, exempt, &violations)
	})
	walkGoFilesUnder(t, root, "pkg", nil, func(_ *token.FileSet, _ *ast.File, relPath string) {
		checkFileLineLimit(t, root, relPath, exempt, &violations)
	})

	for _, v := range violations {
		t.Errorf("inv_FileLineLimit VIOLATED: %s", v)
	}
}

func checkFileLineLimit(t *testing.T, root, relPath string, exempt map[string]bool, violations *[]violation) {
	t.Helper()
	for _, suf := range fileLineLimitExemptSuffixes {
		if strings.HasSuffix(relPath, suf) {
			return
		}
	}
	if exempt[relPath] {
		return
	}
	full := filepath.Join(root, relPath)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read %s: %v", full, err)
	}
	lines := strings.Count(string(data), "\n")
	if lines > fileLineLimitMax {
		*violations = append(*violations, violation{
			relPath: relPath,
			line:    1,
			detail: fmt.Sprintf(
				"文件 %d 行，超过 R7 上限 %d 行 — 须按职责拆分；"+
					"若属于已知存量债务，须登记进 internal/lint/testdata/%s 并附拆分计划，不得无理由静默豁免",
				lines, fileLineLimitMax, fileLineLimitBaselinePath),
		})
	}
}
