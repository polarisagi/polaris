package sleeptime

import (
	"context"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// MineFailures 从 llm_calls 历史记录中挖掘失败模式并聚类为弱点列表。
func MineFailures(ctx context.Context, db protocol.SQLQuerier, limit int) ([]Weakness, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}

	query := `
		SELECT purpose, error
		FROM llm_calls
		WHERE status IN ('error', 'cancelled') OR (error IS NOT NULL AND error != '')
		ORDER BY created_at DESC
		LIMIT ?
	`
	rows, err := db.QueryContext(ctx, query, limit)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime.MineFailures: query llm_calls", err)
	}
	defer rows.Close()

	clusters := make(map[string]*Weakness)
	for rows.Next() {
		var purpose, errStr string
		if err := rows.Scan(&purpose, &errStr); err != nil {
			continue
		}
		if purpose == "" {
			purpose = "unknown"
		}
		cat := classifyError(errStr)
		sig := purpose + ":" + cat

		w, exists := clusters[sig]
		if !exists {
			w = &Weakness{
				Signature:    sig,
				Purpose:      purpose,
				Category:     cat,
				Count:        0,
				SampleErrors: make([]string, 0, 3),
			}
			clusters[sig] = w
		}
		w.Count++
		if len(w.SampleErrors) < 3 && errStr != "" {
			alreadyHas := false
			for _, s := range w.SampleErrors {
				if s == errStr {
					alreadyHas = true
					break
				}
			}
			if !alreadyHas {
				w.SampleErrors = append(w.SampleErrors, errStr)
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sleeptime.MineFailures: rows error", err)
	}

	weaknesses := make([]Weakness, 0, len(clusters))
	for _, w := range clusters {
		weaknesses = append(weaknesses, *w)
	}
	sort.Slice(weaknesses, func(i, j int) bool {
		return weaknesses[i].Count > weaknesses[j].Count
	})

	if len(weaknesses) > 5 {
		weaknesses = weaknesses[:5]
	}
	return weaknesses, nil
}

func classifyError(errStr string) string {
	lower := strings.ToLower(errStr)
	switch {
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline"):
		return "timeout"
	case strings.Contains(lower, "cancel"):
		return "canceled"
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "429") || strings.Contains(lower, "quota"):
		return "rate_limit"
	case strings.Contains(lower, "unmarshal") || strings.Contains(lower, "json") || strings.Contains(lower, "syntax") || strings.Contains(lower, "eof"):
		return "schema_error"
	case strings.Contains(lower, "forbidden") || strings.Contains(lower, "permission") || strings.Contains(lower, "denied") || strings.Contains(lower, "blocked"):
		return "permission_denied"
	case strings.Contains(lower, "not found") || strings.Contains(lower, "no such file"):
		return "not_found"
	case strings.Contains(lower, "exit status") || strings.Contains(lower, "command failed"):
		return "command_failed"
	default:
		return "execution_error"
	}
}
