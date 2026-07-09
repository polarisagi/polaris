//go:build cgo

package knowledge

import (
	"strings"
	"testing"
)

// Test_CodeChunker_TreeSitter_HandlesCommentedFuncKeyword — [Task 15] 核心升级点回归测试。
//
// 2026-07-04 审计修复：原 TestCodeChunker 的样本（"func Test(){}\n\nclass MyClass{}"）
// 没有嵌套/注释干扰，在 tree-sitter 路径和字符串匹配 fallback 路径下结果完全一致，
// 无法验证任务15要求的"字符串匹配版本会切错的场景"这一验收标准。
//
// 本测试构造一段块注释内含 "func " 字样的真实代码——这正是任务文档点名的失败场景
// （"注释里出现的 'func ' 字样"）：
//   - fallbackChunk 按行前缀匹配，注释内的 "func nestedInComment() {" 这一行会被
//     误判为函数边界，产生一次虚假切分（4 个 chunk，且注释被从其归属的顶层节点中
//     割裂）。
//   - treeSitterChunk 基于 AST 只在真正的 function_declaration/method_declaration/
//     type_declaration 顶层节点处切分，注释整体作为非边界节点随前文一起保留在同一
//     chunk 内（3 个 chunk，Real/Another 各自成块，不受注释内容影响）。
func Test_CodeChunker_TreeSitter_HandlesCommentedFuncKeyword(t *testing.T) {
	content := `package sample

/*
func nestedInComment() {
    example code inside a comment, must not be treated as a chunk boundary
}
*/
func Real() {
	x := 1
	return x
}

func Another() {
	return 2
}
`
	c := &CodeChunker{}

	// tree-sitter AST 路径：应正确识别 2 个顶层函数声明，注释不产生虚假边界。
	astChunks := c.Chunk(content, "go")
	if len(astChunks) != 3 {
		t.Fatalf("tree-sitter path: expected 3 chunks (package+comment, func Real, func Another), got %d: %v", len(astChunks), astChunks)
	}
	if !strings.Contains(astChunks[0], "nestedInComment") {
		t.Errorf("tree-sitter path: comment text should stay merged with the leading package clause, chunk[0]=%q", astChunks[0])
	}
	if !strings.Contains(astChunks[1], "func Real") || strings.Contains(astChunks[1], "nestedInComment") {
		t.Errorf("tree-sitter path: chunk[1] should be a clean 'func Real' declaration without comment bleed, got %q", astChunks[1])
	}
	if !strings.Contains(astChunks[2], "func Another") {
		t.Errorf("tree-sitter path: chunk[2] should contain 'func Another', got %q", astChunks[2])
	}

	// 字符串匹配 fallback 路径（同一份代码，直接调用 fallbackChunk）：
	// 应能复现"切错"——注释内的 "func " 字样触发虚假边界，产生 4 个 chunk。
	fallbackChunks := c.fallbackChunk(content)
	if len(fallbackChunks) != 4 {
		t.Fatalf("fallback path: expected naive string-matching to mis-split into 4 chunks (proving the bug this upgrade fixes), got %d: %v", len(fallbackChunks), fallbackChunks)
	}
	if !strings.Contains(fallbackChunks[1], "nestedInComment") {
		t.Errorf("fallback path: expected the comment's 'func ' line to have spuriously started its own chunk, chunk[1]=%q", fallbackChunks[1])
	}

	// 两条路径切出的 chunk 数量必须不同，证明升级前后行为确实存在差异，
	// 而不是恰好又走到了同一个结果。
	if len(astChunks) == len(fallbackChunks) {
		t.Errorf("expected tree-sitter and fallback chunk counts to differ for this sample, both got %d", len(astChunks))
	}
}
