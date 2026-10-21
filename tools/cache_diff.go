//go:build ignore

// cache_diff 比较两次 LLM 请求体（POLARIS_LLM_RAW_DUMP_FULL=1 落盘的 *.req.json），
// 报告前缀缓存在哪里失配（ADR-0120 决策八）。前缀缓存按字节前缀匹配，第一处分歧之后的
// 输入全部按未命中计价，所以只需定位"第一处"。
//
// 用法：
//
//	go run tools/cache_diff.go <earlier.req.json> <later.req.json>
//
// 不进 CI：它是排障工具，不是门控。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

type req struct {
	Model    string            `json:"model"`
	Tools    json.RawMessage   `json:"tools"`
	Messages []json.RawMessage `json:"messages"`
}

func load(path string) req {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var r req
	if err := json.Unmarshal(b, &r); err != nil {
		fmt.Fprintln(os.Stderr, path, err)
		os.Exit(2)
	}
	return r
}

func compact(b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return b
	}
	return buf.Bytes()
}

func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

func window(b []byte, at int) string {
	lo, hi := max(0, at-80), min(len(b), at+80)
	return string(b[lo:at]) + "⟦|⟧" + string(b[at:hi])
}

func role(m []byte) string {
	var v struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(m, &v)
	return v.Role
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run tools/cache_diff.go <earlier.req.json> <later.req.json>")
		os.Exit(2)
	}
	a, b := load(os.Args[1]), load(os.Args[2])
	if a.Model != b.Model {
		fmt.Printf("model differs: %q -> %q (KV cache is per model)\n", a.Model, b.Model)
	}
	ta, tb := compact(a.Tools), compact(b.Tools)
	if d := firstDiff(ta, tb); d >= 0 {
		fmt.Printf("TOOLS differ at byte %d (everything after tools is a miss)\n  a: %s\n  b: %s\n", d, window(ta, min(d, len(ta))), window(tb, min(d, len(tb))))
		return
	}
	fmt.Printf("tools identical (%d bytes)\n", len(ta))
	for i := 0; i < len(a.Messages); i++ {
		if i >= len(b.Messages) {
			fmt.Printf("later request has fewer messages (%d < %d)\n", len(b.Messages), len(a.Messages))
			return
		}
		ma, mb := compact(a.Messages[i]), compact(b.Messages[i])
		if d := firstDiff(ma, mb); d >= 0 {
			fmt.Printf("messages[%d] (role %s/%s) differ at byte %d\n  a: %s\n  b: %s\n", i, role(ma), role(mb), d, window(ma, min(d, len(ma))), window(mb, min(d, len(mb))))
			if i == len(a.Messages)-1 {
				fmt.Println("  (this is the earlier request's last message — usually the per-call volatile tail; that is expected)")
			}
			return
		}
	}
	fmt.Printf("earlier request is a full prefix of the later one (%d of %d messages shared)\n", len(a.Messages), len(b.Messages))
}
