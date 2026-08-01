package lint_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// ─── inv_HE1_NoSilentErrorDiscard ────────────────────────────────────────────
//
// 阶段02（02-error-handling.md §3）新增：分级 errcheck-style lint 门控，检测
// `_ = f(...)` / `_, _ = f(...)` 形式的静默错误吞没（对应 HE-1 可观测优先，
// 见 docs/arch/00-Global-Dictionary.md）。
//
// 设计取舍（与本文件其余规则一致，纯 go/ast 静态扫描，不接入 go/types）：
//   - 只匹配「左值全为 `_`」且「右值至少一个是函数/方法调用」的赋值语句，
//     排除 `_ = someVar // 标记故意未使用` 这种非调用丢弃（不是错误吞没）。
//   - 按被调用方法/函数的裸名匹配 errcheck_exempt.json（无法解析接收者静态
//     类型，见该文件 _comment 说明）。
//   - 目录白名单 errcheck_enforced_dirs.json：未接入目录不扫描，非豁免。
//   - baseline 快照 errcheck_baseline.json：阶段02 收尾时已存在但判定为
//     "超出本阶段命名范围、留待后续批次"的调用点（见
//     local_playground/upgrade/99-new-findings.md 对应记录）予以豁免，
//     防止规则一启用就报出与本阶段无关的历史存量。新增代码不受 baseline
//     保护——这是"棘轮"（ratchet）语义：只挡新增，不追溯旧账。

// silentDiscard 描述一处 "_ = f(...)" / "_, _ = f(...)" 形式的调用发现。
type silentDiscard struct {
	relPath  string
	line     int
	callName string // 被丢弃返回值的方法/函数裸名（无法识别时为空串）
}

func (d silentDiscard) key() string {
	return fmt.Sprintf("%s:%d", d.relPath, d.line)
}

func (d silentDiscard) String() string {
	return fmt.Sprintf("%s: 静默丢弃调用返回值（`_ = ...`/`_, _ = ...`），需按 HE-1 分级（L1-L4）处理——callName=%s", d.key(), d.callName)
}

// silentDiscardCallName 返回调用表达式的裸方法/函数名（不含包名/接收者），
// 无法识别时返回空串。
func silentDiscardCallName(fun ast.Expr) string {
	switch e := fun.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.Ident:
		return e.Name
	default:
		return ""
	}
}

// collectSilentErrorDiscards 在 root/dir 下（跳过 _test.go/.pb.go）查找所有
// "左值全为 `_`，右值含至少一个调用表达式" 的赋值语句。
func collectSilentErrorDiscards(t *testing.T, root, dir string) []silentDiscard {
	t.Helper()
	var found []silentDiscard
	walkGoFilesUnder(t, root, dir, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || assign.Tok != token.ASSIGN {
				return true
			}
			allBlank := len(assign.Lhs) > 0
			for _, lhs := range assign.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != "_" {
					allBlank = false
					break
				}
			}
			if !allBlank {
				return true
			}
			var callName string
			hasCall := false
			for _, rhs := range assign.Rhs {
				if call, ok := rhs.(*ast.CallExpr); ok {
					hasCall = true
					callName = silentDiscardCallName(call.Fun)
				}
			}
			if !hasCall {
				return true
			}
			pos := fset.Position(assign.Pos())
			found = append(found, silentDiscard{relPath: relPath, line: pos.Line, callName: callName})
			return true
		})
	})
	return found
}

func Test_inv_HE1_NoSilentErrorDiscard(t *testing.T) {
	root := repoRoot(t)

	exemptNames := loadExemptFile(t, root, "errcheck_exempt.json")
	enforcedDirs := loadExemptFile(t, root, "errcheck_enforced_dirs.json")
	baseline := loadExemptFileRaw(t, root, "errcheck_baseline.json")

	dirs := make([]string, 0, len(enforcedDirs))
	for d := range enforcedDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var violations []silentDiscard
	for _, dir := range dirs {
		for _, d := range collectSilentErrorDiscards(t, root, dir) {
			if exemptNames[d.callName] {
				continue
			}
			if baseline[d.key()] {
				continue // 阶段02 结束时已存在、登记为后续批次处理的存量，棘轮豁免
			}
			violations = append(violations, d)
		}
	}

	for _, v := range violations {
		t.Errorf("inv_HE1_NoSilentErrorDiscard VIOLATED: %s", v)
	}
}

// TestGenerateErrcheckBaseline 是 errcheck_baseline.json 的生成器，默认跳过
// （不参与 `make test`/`make lint`）。仅在人工扩展 errcheck_enforced_dirs.json
// 接入新目录、需要为该目录的历史存量重新生成 baseline 快照时，手动执行：
//
//	POLARIS_GEN_ERRCHECK_BASELINE=1 go test ./internal/lint/ -run TestGenerateErrcheckBaseline -v
//
// 生成后必须人工审查新增的 baseline 条目（每条都应能在 99-new-findings.md
// 找到对应的"超出范围、留待后续批次"记录），禁止盲目跑一遍就提交——baseline
// 是"记录已知存量"而非"批量豁免新写的代码"。
func TestGenerateErrcheckBaseline(t *testing.T) {
	if os.Getenv("POLARIS_GEN_ERRCHECK_BASELINE") != "1" {
		t.Skip("baseline 生成器默认跳过，设置 POLARIS_GEN_ERRCHECK_BASELINE=1 手动运行")
	}
	root := repoRoot(t)
	enforcedDirs := loadExemptFile(t, root, "errcheck_enforced_dirs.json")
	exemptNames := loadExemptFile(t, root, "errcheck_exempt.json")

	dirs := make([]string, 0, len(enforcedDirs))
	for d := range enforcedDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	out := make(map[string]bool)
	for _, dir := range dirs {
		for _, d := range collectSilentErrorDiscards(t, root, dir) {
			if exemptNames[d.callName] {
				continue
			}
			out[d.key()] = true
		}
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	p := filepath.Join(root, "internal", "lint", "testdata", "errcheck_baseline.json")
	if err := os.WriteFile(p, b, 0644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	t.Logf("wrote %d baseline entries to %s", len(out), p)
}
