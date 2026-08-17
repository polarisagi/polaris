package lintutil

import (
	"fmt"
	"os"
	"strings"
)

// BaselineDir 是全仓抑制表（棘轮基线 / 豁免白名单）的唯一落点。
// 判准见该目录的 README.md：抑制存量的进这里，规则输入表留在 tools/ 下与规则同放。
const BaselineDir = "tools/baselines"

// KeySet 是一张抑制表。nil KeySet 合法，表示「不接受任何存量」（fail-closed 规则）。
type KeySet map[string]bool

// Has 报告 key 是否被抑制。nil receiver 安全。
func (s KeySet) Has(key string) bool { return s != nil && s[key] }

// Len 返回条目数。
func (s KeySet) Len() int { return len(s) }

// LoadBaseline 从 tools/baselines/<name> 读取抑制表。
//
// 统一格式：每行取**第一个空白分隔的 token** 作为键，其余视为理由文字；`#` 开头与
// 空行忽略；Markdown 列表前缀 `- ` 与行尾冒号会被剥掉。这样同一个解析器既能吃
// 纯清单（`path:line`），也能吃带说明的 .md 基线，而抑制表始终可以写理由——
// 「白名单要审计不要清空」要求每条都能说清为什么合法，格式不该成为借口。
//
// 文件不存在返回空表且不报错：新规则首次接入时基线尚不存在是正常状态。
// 但**存在却读不动**是门控失效，直接 exit 2——静默当成空表会让抑制悄悄失灵
// （2026-08-17 untrack local_playground 一次废掉 6 条规则，就是这么发生的）。
func LoadBaseline(name string) KeySet {
	path := BaselineDir + "/" + name
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return KeySet{}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "lintutil: 抑制表 %s 存在但读取失败: %v\n", path, err)
		os.Exit(2)
	}
	return parseBaseline(string(data))
}

func parseBaseline(text string) KeySet {
	out := KeySet{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if !looksLikeKey(key) {
			continue
		}
		out[key] = true
	}
	return out
}

// looksLikeKey 过滤掉基线文件里的散文行：键必须以某个扫描根开头，或形如 path:line。
func looksLikeKey(s string) bool {
	for _, root := range ScanRoots() {
		if strings.HasPrefix(s, root+"/") {
			return true
		}
	}
	return strings.HasPrefix(s, "rust/") || strings.HasPrefix(s, "docs/")
}
