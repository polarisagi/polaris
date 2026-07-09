package data_query

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

//nolint:gocyclo
func MakeDataQueryFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Query    string `json:"query"`
			Database string `json:"database"`
			Params   []any  `json:"params"`
			MaxRows  int    `json:"max_rows"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.New(apperr.CodeInternal, "data_query: invalid input JSON")
		}

		// 行数上限校验
		if args.MaxRows <= 0 {
			args.MaxRows = 1000
		}
		if args.MaxRows > 10000 {
			args.MaxRows = 10000
		}

		// 路径白名单校验（与 read_file 共用机制）
		if err := guard.CheckAllowedPath(args.Database, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeForbidden, "data_query: database path not allowed", err)
		}

		// SELECT-only 校验（R1.15）：
		// 1. 先剥离 SQL 注释（防 /* DROP TABLE */ SELECT ... 绕过）
		// 2. 首 token 必须为 SELECT 或 WITH（CTE 语法）；WITH 必须包含 SELECT
		// 3. 禁止多语句（分号 = 潜在第二条 DDL/DML）
		normalized := stripSQLComments(args.Query)
		trimmed := strings.ToUpper(strings.TrimSpace(normalized))
		if trimmed == "" {
			return nil, apperr.New(apperr.CodeForbidden, "data_query: empty query after stripping comments")
		}
		fields := strings.Fields(trimmed)
		firstKw := fields[0]
		if firstKw != "SELECT" && firstKw != "WITH" {
			preview := trimmed
			if len(preview) > 20 {
				preview = preview[:20] + "..."
			}
			return nil, apperr.New(apperr.CodeForbidden,
				"data_query: only SELECT/WITH...SELECT queries are permitted (got: "+preview+")")
		}
		if firstKw == "WITH" && !strings.Contains(trimmed, "SELECT") {
			return nil, apperr.New(apperr.CodeForbidden, "data_query: WITH clause must contain SELECT")
		}
		if strings.ContainsRune(trimmed, ';') {
			return nil, apperr.New(apperr.CodeForbidden, "data_query: multi-statement queries not permitted")
		}

		// 只读连接（mode=ro 阻止任何写操作在 OS 层）
		dbURI := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", filepath.Clean(args.Database))
		db, err := sql.Open("sqlite", dbURI)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: open db failed", err)
		}
		defer db.Close()

		// 单连接只读，无需连接池
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)

		// 30 秒查询超时
		qCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		rows, err := db.QueryContext(qCtx, args.Query, args.Params...)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: query failed", err)
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: get columns failed", err)
		}

		// 收集结果行
		result := make([]map[string]any, 0, min(args.MaxRows, 64))
		truncated := false
		rowCount := 0
		vals := make([]any, len(cols))
		valPtrs := make([]any, len(cols))
		for i := range vals {
			valPtrs[i] = &vals[i]
		}

		for rows.Next() {
			if rowCount >= args.MaxRows {
				truncated = true
				break
			}
			if err := rows.Scan(valPtrs...); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "data_query: scan row failed", err)
			}
			row := make(map[string]any, len(cols))
			for i, col := range cols {
				// SQLite 返回 []byte 时转 string，便于 JSON 序列化
				switch v := vals[i].(type) {
				case []byte:
					row[col] = string(v)
				default:
					row[col] = v
				}
			}
			result = append(result, row)
			rowCount++
		}
		if err := rows.Err(); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: rows iteration failed", err)
		}

		out, err := json.Marshal(map[string]any{
			"rows":      result,
			"count":     rowCount,
			"truncated": truncated,
			"columns":   cols,
		})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: marshal result failed", err)
		}
		return out, nil
	}
}

func stripSQLComments(s string) string {
	// 1. 移除块注释 /* ... */（不支持嵌套，符合 SQL 标准）
	var buf strings.Builder
	buf.Grow(len(s))
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			j := i + 2
			for j+1 < len(s) && (s[j] != '*' || s[j+1] != '/') {
				j++
			}
			if j+1 < len(s) {
				j += 2 // 跳过 */
			} else {
				j = len(s) // 未关闭的块注释到末尾
			}
			buf.WriteByte(' ') // 占位保持 token 边界
			i = j
			continue
		}
		buf.WriteByte(s[i])
		i++
	}
	// 2. 移除行注释 -- ...（直到换行符）
	lines := strings.Split(buf.String(), "\n")
	for idx, line := range lines {
		if ci := strings.Index(line, "--"); ci >= 0 {
			lines[idx] = line[:ci]
		}
	}
	return strings.Join(lines, "\n")
}
