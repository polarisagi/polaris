// Package loopguard 提供 agentic 回合的"无进展"检测（ADR-0020 repeat-tool-reminder 复核，2026-10-28）。
//
// 无进展 = 同一工具 + 同一规范化参数 + 同一结果 反复出现。只比较调用而不比较结果会误伤合法重复：
// 编辑后重读同一文件、轮询任务状态，参数相同但结果在变，这是在推进而不是在打转。
// 包内无 IO、无全局状态，便于 Eval 直接重放真实轨迹（internal/eval/harness/repeat_eval_test.go）。
package loopguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
)

// Level 检测结论的严重度，由低到高。
type Level int

const (
	None Level = iota
	// Warn 需要在易变上下文末尾提醒模型换路径或收尾。
	Warn
	// Stop 提醒之后仍在原地打转，应收尾。
	Stop
)

// Finding 单次观察的结论。
type Finding struct {
	Level Level
	Tool  string
	// Count 该调用已连续得到相同结果的累计次数（含本次）。
	Count int
}

// Detector 在一个回合内累计同调用同结果的次数。非并发安全，由 runExecuteLoop 串行使用。
type Detector struct {
	warnAt, stopAt int
	seen           map[[32]byte]*entry
}

type entry struct {
	result [32]byte
	count  int
}

// New 创建检测器。warnAt<=0 关闭检测；stopAt<=0 只提醒不收尾。
func New(warnAt, stopAt int) *Detector {
	return &Detector{warnAt: warnAt, stopAt: stopAt, seen: map[[32]byte]*entry{}}
}

// Observe 记录一次已完成的工具调用。同调用的结果变化会把计数重置为 1。
func (d *Detector) Observe(tool string, args []byte, result string) Finding {
	if d == nil || d.warnAt <= 0 || Exempt(tool) {
		return Finding{Tool: tool}
	}
	key := sha256.Sum256(append([]byte(tool+"\x00"), canonicalArgs(args)...))
	res := sha256.Sum256([]byte(result))

	e, ok := d.seen[key]
	switch {
	case !ok:
		e = &entry{result: res, count: 1}
		d.seen[key] = e
	case e.result == res:
		e.count++
	default:
		e.result, e.count = res, 1
	}

	f := Finding{Tool: tool, Count: e.count}
	switch {
	case d.stopAt > 0 && e.count >= d.stopAt:
		f.Level = Stop
	case e.count >= d.warnAt:
		f.Level = Warn
	}
	return f
}

// Exempt 判断工具是否属于合法轮询类。Stop 对它们不适用，否则会把"等任务跑完"的长任务误杀。
func Exempt(tool string) bool {
	t := strings.ToLower(tool)
	// 名称含这些片段的工具是"等待外部状态变化"类，相同结果是预期内的（如反复得到 running）。
	for _, tok := range [...]string{"status", "poll", "wait", "sleep", "watch", "tail", "progress", "ping", "health"} {
		if strings.Contains(t, tok) {
			return true
		}
	}
	return false
}

// canonicalArgs 把 JSON 参数规范化（键排序、去空白），使键顺序不同的等价调用得到同一哈希。
// 非 JSON 内容退化为去首尾空白的原文。数字保留原文（UseNumber），避免 1.0 与 1 被误判相同或大整数失真。
func canonicalArgs(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}
