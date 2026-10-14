package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// DedupState 跨运行去重状态接口（由 Runner / 仓储层提供）。
type DedupState interface {
	Seen(keys []string) (map[string]bool, error)
	Pending(keys []string)
}

// ApplyOps 依次对输入数据应用 transform 算子序列。
func ApplyOps(in StepData, ops []Op, inputs map[string]StepData, dedup DedupState) (StepData, error) {
	curr := in

	for _, op := range ops {
		var err error
		switch op.Op {
		case "concat":
			curr, err = opConcat(op, inputs)
		case "dedup":
			curr, err = opDedup(curr, op, dedup)
		case "filter":
			curr, err = opFilter(curr, op)
		case "sort":
			curr, err = opSort(curr, op)
		case "limit":
			curr = opLimit(curr, op)
		case "pick":
			curr = opPick(curr, op)
		case "rename":
			curr = opRename(curr, op)
		case "truncate":
			curr = opTruncate(curr, op)
		default:
			return curr, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("pipeline: unknown op %q", op.Op))
		}
		if err != nil {
			return curr, err
		}
	}

	curr.Count = len(curr.Items)
	return curr, nil
}

func opConcat(op Op, inputs map[string]StepData) (StepData, error) {
	var merged []map[string]any
	for _, fromKey := range op.From {
		if input, ok := inputs[fromKey]; ok {
			for _, item := range input.Items {
				merged = append(merged, cloneMap(item))
			}
		}
	}
	return StepData{
		Items: merged,
		Count: len(merged),
	}, nil
}

func opDedup(in StepData, op Op, dedup DedupState) (StepData, error) {
	if len(in.Items) == 0 {
		return in, nil
	}

	keys := make([]string, len(in.Items))
	for i, item := range in.Items {
		val, ok := getPath(item, op.By)
		if ok && val != nil && fmt.Sprint(val) != "" {
			h := sha256.Sum256([]byte(fmt.Sprint(val)))
			keys[i] = hex.EncodeToString(h[:16]) // 前 32 hex
		}
	}

	seenWorkflow := make(map[string]bool)
	if op.Scope == "workflow" && dedup != nil {
		var validKeys []string
		for _, k := range keys {
			if k != "" {
				validKeys = append(validKeys, k)
			}
		}
		if len(validKeys) > 0 {
			res, err := dedup.Seen(validKeys)
			if err != nil {
				return in, apperr.Wrap(apperr.CodeInternal, "pipeline: dedup seen query failed", err)
			}
			seenWorkflow = res
		}
	}

	var out []map[string]any
	seenLocal := make(map[string]bool)
	var newKeys []string

	for i, item := range in.Items {
		k := keys[i]
		if k == "" {
			// 缺失去重字段的条目保留
			out = append(out, item)
			continue
		}

		if seenLocal[k] || seenWorkflow[k] {
			continue
		}

		seenLocal[k] = true
		out = append(out, item)
		if op.Scope == "workflow" {
			newKeys = append(newKeys, k)
		}
	}

	if op.Scope == "workflow" && dedup != nil && len(newKeys) > 0 {
		dedup.Pending(newKeys)
	}

	return StepData{
		Items:     out,
		Text:      in.Text,
		Count:     len(out),
		Truncated: in.Truncated,
	}, nil
}

func opFilter(in StepData, op Op) (StepData, error) {
	var out []map[string]any
	var regexPattern *regexp.Regexp
	if op.Cmp == "regex" {
		patternStr, _ := op.Value.(string)
		re, err := regexp.Compile(patternStr)
		if err != nil {
			return in, apperr.Wrap(apperr.CodeInvalidInput, "pipeline: invalid regex", err)
		}
		regexPattern = re
	}

	for _, item := range in.Items {
		val, ok := getPath(item, op.Field)
		if !ok || val == nil {
			if op.KeepMissing {
				out = append(out, item)
			}
			continue
		}

		match := evalFilter(val, op.Cmp, op.Value, regexPattern)
		if match {
			out = append(out, item)
		}
	}

	return StepData{
		Items:     out,
		Text:      in.Text,
		Count:     len(out),
		Truncated: in.Truncated,
	}, nil
}

func evalFilter(actual any, cmp string, expected any, re *regexp.Regexp) bool {
	switch cmp {
	case "contains":
		actStr := strings.ToLower(fmt.Sprint(actual))
		expStr := strings.ToLower(fmt.Sprint(expected))
		return strings.Contains(actStr, expStr)
	case "not_contains":
		actStr := strings.ToLower(fmt.Sprint(actual))
		expStr := strings.ToLower(fmt.Sprint(expected))
		return !strings.Contains(actStr, expStr)
	case "regex":
		if re == nil {
			return false
		}
		return re.MatchString(fmt.Sprint(actual))
	}

	// 比较数值
	actNum, ok1 := toFloat64(actual)
	expNum, ok2 := toFloat64(expected)
	if ok1 && ok2 {
		switch cmp {
		case "==":
			return actNum == expNum
		case "!=":
			return actNum != expNum
		case ">":
			return actNum > expNum
		case ">=":
			return actNum >= expNum
		case "<":
			return actNum < expNum
		case "<=":
			return actNum <= expNum
		}
	}

	// 比较时间（RFC3339）
	actTime, err1 := time.Parse(time.RFC3339, fmt.Sprint(actual))
	expTime, err2 := time.Parse(time.RFC3339, fmt.Sprint(expected))
	if err1 == nil && err2 == nil {
		switch cmp {
		case "==":
			return actTime.Equal(expTime)
		case "!=":
			return !actTime.Equal(expTime)
		case ">":
			return actTime.After(expTime)
		case ">=":
			return actTime.After(expTime) || actTime.Equal(expTime)
		case "<":
			return actTime.Before(expTime)
		case "<=":
			return actTime.Before(expTime) || actTime.Equal(expTime)
		}
	}

	// 兜底字符串比较
	s1 := fmt.Sprint(actual)
	s2 := fmt.Sprint(expected)
	switch cmp {
	case "==":
		return s1 == s2
	case "!=":
		return s1 != s2
	case ">":
		return s1 > s2
	case ">=":
		return s1 >= s2
	case "<":
		return s1 < s2
	case "<=":
		return s1 <= s2
	default:
		return false
	}
}

func opSort(in StepData, op Op) (StepData, error) {
	items := make([]map[string]any, len(in.Items))
	copy(items, in.Items)

	sort.SliceStable(items, func(i, j int) bool {
		valI, okI := getPath(items[i], op.By)
		valJ, okJ := getPath(items[j], op.By)

		// 缺失字段排在最后
		if !okI && !okJ {
			return false
		}
		if !okI {
			return false
		}
		if !okJ {
			return true
		}

		less := compareLess(valI, valJ)
		if op.Desc {
			return !less && !compareEqual(valI, valJ)
		}
		return less
	})

	return StepData{
		Items:     items,
		Text:      in.Text,
		Count:     len(items),
		Truncated: in.Truncated,
	}, nil
}

func compareLess(a, b any) bool {
	// 数值
	n1, ok1 := toFloat64(a)
	n2, ok2 := toFloat64(b)
	if ok1 && ok2 {
		return n1 < n2
	}

	// 时间
	t1, err1 := time.Parse(time.RFC3339, fmt.Sprint(a))
	t2, err2 := time.Parse(time.RFC3339, fmt.Sprint(b))
	if err1 == nil && err2 == nil {
		return t1.Before(t2)
	}

	// 字符串
	return fmt.Sprint(a) < fmt.Sprint(b)
}

func compareEqual(a, b any) bool {
	n1, ok1 := toFloat64(a)
	n2, ok2 := toFloat64(b)
	if ok1 && ok2 {
		return n1 == n2
	}
	t1, err1 := time.Parse(time.RFC3339, fmt.Sprint(a))
	t2, err2 := time.Parse(time.RFC3339, fmt.Sprint(b))
	if err1 == nil && err2 == nil {
		return t1.Equal(t2)
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func opLimit(in StepData, op Op) StepData {
	if op.N <= 0 || len(in.Items) <= op.N {
		return in
	}
	items := in.Items[:op.N]
	return StepData{
		Items:     items,
		Text:      in.Text,
		Count:     len(items),
		Truncated: in.Truncated,
	}
}

func opPick(in StepData, op Op) StepData {
	out := make([]map[string]any, len(in.Items))
	for i, item := range in.Items {
		picked := make(map[string]any)
		for _, f := range op.Fields {
			if val, ok := getPath(item, f); ok {
				setPath(picked, f, val)
			}
		}
		out[i] = picked
	}
	return StepData{
		Items:     out,
		Text:      in.Text,
		Count:     len(out),
		Truncated: in.Truncated,
	}
}

func opRename(in StepData, op Op) StepData {
	out := make([]map[string]any, len(in.Items))
	for i, item := range in.Items {
		m := cloneMap(item)
		for oldPath, newPath := range op.Map {
			if val, ok := getPath(m, oldPath); ok {
				setPath(m, newPath, val)
				deletePath(m, oldPath)
			}
		}
		out[i] = m
	}
	return StepData{
		Items:     out,
		Text:      in.Text,
		Count:     len(out),
		Truncated: in.Truncated,
	}
}

func opTruncate(in StepData, op Op) StepData {
	out := make([]map[string]any, len(in.Items))
	for i, item := range in.Items {
		m := cloneMap(item)
		if val, ok := getPath(m, op.Field); ok {
			if s, isStr := val.(string); isStr {
				runes := []rune(s)
				if op.MaxChars > 0 && len(runes) > op.MaxChars {
					setPath(m, op.Field, string(runes[:op.MaxChars]))
				}
			}
		}
		out[i] = m
	}
	return StepData{
		Items:     out,
		Text:      in.Text,
		Count:     len(out),
		Truncated: in.Truncated,
	}
}

// ─── 点路径辅助函数 ─────────────────────────────────────────────────────────────

func getPath(m map[string]any, path string) (any, bool) {
	if m == nil {
		return nil, false
	}
	parts := strings.Split(path, ".")
	var curr any = m
	for _, p := range parts {
		asMap, ok := curr.(map[string]any)
		if !ok {
			return nil, false
		}
		val, exists := asMap[p]
		if !exists {
			return nil, false
		}
		curr = val
	}
	return curr, true
}

func setPath(m map[string]any, path string, val any) {
	parts := strings.Split(path, ".")
	curr := m
	for i := 0; i < len(parts)-1; i++ {
		p := parts[i]
		next, ok := curr[p].(map[string]any)
		if !ok {
			next = make(map[string]any)
			curr[p] = next
		}
		curr = next
	}
	curr[parts[len(parts)-1]] = val
}

func deletePath(m map[string]any, path string) {
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		delete(m, parts[0])
		return
	}
	curr := m
	for i := 0; i < len(parts)-1; i++ {
		p := parts[i]
		next, ok := curr[p].(map[string]any)
		if !ok {
			return
		}
		curr = next
	}
	delete(curr, parts[len(parts)-1])
}

func cloneMap(m map[string]any) map[string]any {
	res := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			res[k] = cloneMap(sub)
		} else {
			res[k] = v
		}
	}
	return res
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
}
