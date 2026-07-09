package csv_parse

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type csvParseArgs struct {
	CSV string `json:"csv"`
}

func CsvParseFn(_ context.Context, input []byte) ([]byte, error) {
	var args csvParseArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "csv_parse: invalid args", err)
	}
	if args.CSV == "" {
		return nil, apperr.New(apperr.CodeInternal, "csv_parse: csv is required")
	}

	lines := strings.Split(strings.ReplaceAll(args.CSV, "\r\n", "\n"), "\n")
	// 过滤空行
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) < 2 {
		return json.Marshal([]map[string]string{}) //nolint:wrapcheck
	}

	// 解析表头
	headers := splitCSVLine(nonEmpty[0])
	rows := make([]map[string]string, 0, len(nonEmpty)-1)
	for _, line := range nonEmpty[1:] {
		cols := splitCSVLine(line)
		row := make(map[string]string, len(headers))
		for i, h := range headers {
			if i < len(cols) {
				row[h] = cols[i]
			} else {
				row[h] = ""
			}
		}
		rows = append(rows, row)
	}
	return json.Marshal(rows) //nolint:wrapcheck
}

func splitCSVLine(line string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"' && !inQuote:
			inQuote = true
		case c == '"' && inQuote:
			// 连续两个引号 → 转义单引号
			if i+1 < len(line) && line[i+1] == '"' {
				cur.WriteByte('"')
				i++
			} else {
				inQuote = false
			}
		case c == ',' && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	fields = append(fields, cur.String())
	return fields
}
