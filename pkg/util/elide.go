package util

import (
	"strconv"
	"unicode/utf8"
)

// ElideMiddle 把 s 缩短到不超过 maxBytes 字节：保留开头约 3/4 与结尾约 1/4，
// 中间替换为省略标记。
//
// 只保留开头会丢掉结论常在末尾的输出（编译错误、堆栈、测试汇总、命令退出码）；
// 首尾各留一段是 DeepSeek Harness 工具结果保留策略的做法。切点对齐 UTF-8
// 字符边界，maxBytes 不足以容纳标记时退化为纯头部截断。
func ElideMiddle(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	// 标记长度随省略字节数变化；按最坏情况（整个 s 都被省略）预留，保证不超限。
	marker := elisionMarker(len(s))
	if maxBytes <= len(marker) {
		return s[:runeStartBefore(s, maxBytes)]
	}
	keep := maxBytes - len(marker)
	headEnd := runeStartBefore(s, keep-keep/4)
	tailStart := runeStartAfter(s, len(s)-(keep-headEnd))
	return s[:headEnd] + elisionMarker(tailStart-headEnd) + s[tailStart:]
}

func elisionMarker(omitted int) string {
	return "\n...[" + strconv.Itoa(omitted) + " bytes omitted]...\n"
}

// runeStartBefore 返回不大于 i 的最近字符起始偏移。
func runeStartBefore(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeStartAfter 返回不小于 i 的最近字符起始偏移。
func runeStartAfter(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
