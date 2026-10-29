package diff_text

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type diffTextArgs struct {
	Old string `json:"old"`
	New string `json:"new"`
}

func DiffTextFn(_ context.Context, input []byte) ([]byte, error) {
	var args diffTextArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "diff_text: invalid args", err)
	}

	oldLines := strings.Split(args.Old, "\n")
	newLines := strings.Split(args.New, "\n")
	diff := computeUnifiedDiff(oldLines, newLines)

	result := map[string]any{
		"diff":     diff,
		"has_diff": diff != "",
	}
	return json.Marshal(result) //nolint:wrapcheck
}

func computeUnifiedDiff(oldLines, newLines []string) string { //nolint:gocyclo
	// LCS 长度表
	m, n := len(oldLines), len(newLines)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if oldLines[i-1] == newLines[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// 回溯构造差异列表
	type op struct {
		kind byte // ' ' '+' '-'
		line string
	}
	var ops []op
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && oldLines[i-1] == newLines[j-1]:
			ops = append(ops, op{' ', oldLines[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			ops = append(ops, op{'+', newLines[j-1]})
			j--
		default:
			ops = append(ops, op{'-', oldLines[i-1]})
			i--
		}
	}
	// 反转
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}

	// 输出有变化的行（带 context=3）
	const ctx = 3
	changed := make([]bool, len(ops))
	for idx, o := range ops {
		if o.kind != ' ' {
			changed[idx] = true
		}
	}

	var sb strings.Builder
	sb.WriteString("--- old\n+++ new\n")
	printed := make([]bool, len(ops))
	for idx := range ops {
		if !changed[idx] {
			continue
		}
		start := max(idx-ctx, 0)
		end := min(idx+ctx+1, len(ops))
		for k := start; k < end; k++ {
			if printed[k] {
				continue
			}
			printed[k] = true
			switch ops[k].kind {
			case '+':
				sb.WriteString("+" + ops[k].line + "\n")
			case '-':
				sb.WriteString("-" + ops[k].line + "\n")
			default:
				sb.WriteString(" " + ops[k].line + "\n")
			}
		}
	}
	return sb.String()
}
