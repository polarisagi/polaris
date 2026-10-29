// Package util — 通用无业务逻辑工具函数（pkg/ 契约，任意层可引用）。
package util

import "strings"

// ADR-0094 决策六：结构化载体禁字符串直拼——SQLite FTS5 MATCH 表达式有自己的
// 查询语法（"、*、:、AND/OR/NOT、() 等均为语法保留字符），若把任意实体名/用户
// 查询原样拼进 MATCH ? 的参数值，遇到含这些字符的输入会导致 SQLite 语法解析
// 报错（例如实体名里带英文双引号，或用户搜索词恰好是纯大写的 "AND"）。

// quoteFTS5Literal 把单个 token 转义为 FTS5 安全的字面量短语：整体包一层双引号，
// 内部出现的双引号按 SQLite 字符串字面量转义规则加倍（" → ""）。
func quoteFTS5Literal(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// QuoteFTS5 将实体名一类的短标识符转义为单个精确短语匹配。用于 entity name 这类
// "整体作为一个单位比对"更符合语义的场景（graph_traverser.chunksForEntity）。
func QuoteFTS5(s string) string {
	return quoteFTS5Literal(s)
}

// QuoteFTS5Query 将多词自由文本查询转义为安全的 FTS5 表达式：按空白切分为多个
// token，每个 token 单独加引号转义后以空格连接（FTS5 默认对相邻短语做隐式 AND）。
// 相比 QuoteFTS5 整体加引号，这保留了"多词任一命中即参与排序"的 BM25 召回行为
// ——对用户搜索框/RAG 查询这类需要模糊召回的场景，整体精确短语匹配会让召回率
// 塌缩到几乎总是空结果，因此单独提供这一变体。
func QuoteFTS5Query(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return `""`
	}
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = quoteFTS5Literal(f)
	}
	return strings.Join(quoted, " ")
}
