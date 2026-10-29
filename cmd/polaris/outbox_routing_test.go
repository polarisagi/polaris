package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// outbox 按 TargetEngine 路由：写入端与 RegisterHandler 引用的 topic 表达式不一致时，
// 记录会被判 unknown target engine 直接落 dead（卸载曾因此从未执行）。
// 这里静态扫描全仓非测试代码，要求写入端出现的每个 topic 都有同表达式的注册处理器，
// 且两端都不得使用字符串字面量。
func TestOutboxTopicRoutingConsistent(t *testing.T) {
	fset := token.NewFileSet()
	registered := map[string]bool{}
	written := map[string]string{} // expr → 位置

	exprStr := func(e ast.Expr) string {
		var b bytes.Buffer
		_ = printer.Fprint(&b, fset, e)
		return b.String()
	}
	note := func(m map[string]string, e ast.Expr, pos token.Pos, side string) {
		s := exprStr(e)
		if _, lit := e.(*ast.BasicLit); lit {
			t.Errorf("%s 使用字符串字面量 %s（%s），须引用 protocol.TopicXxx 常量", side, s, fset.Position(pos))
			return
		}
		if _, ok := m[s]; !ok {
			m[s] = fset.Position(pos).String()
		}
	}
	regPos := map[string]string{}

	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.Contains(path, "/pb/") {
				return err
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch {
					case sel.Sel.Name == "RegisterHandler" && len(x.Args) >= 2 && strings.Contains(exprStr(sel.X), "Outbox"):
						note(regPos, x.Args[0], x.Pos(), "RegisterHandler")
					case sel.Sel.Name == "NewOutboxEvent" && len(x.Args) >= 1:
						note(written, x.Args[0], x.Pos(), "NewOutboxEvent")
					case sel.Sel.Name == "emitOutbox" && len(x.Args) >= 2:
						note(written, x.Args[1], x.Pos(), "emitOutbox")
					}
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok && k.Name == "TargetEngine" {
						note(written, x.Value, x.Pos(), "OutboxEntry.TargetEngine")
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for k := range regPos {
		registered[k] = true
	}
	if len(registered) == 0 || len(written) == 0 {
		t.Fatalf("扫描结果为空，检测失效: registered=%d written=%d", len(registered), len(written))
	}
	for expr, pos := range written {
		// 变量形态（如 emitOutbox 内部的 topic 形参）无法静态解析，只校验常量选择子表达式
		if !strings.Contains(expr, ".") {
			continue
		}
		if !registered[expr] {
			t.Errorf("topic %s 有写入端（%s）但无 RegisterHandler，记录将落 dead", expr, pos)
		}
	}
}
