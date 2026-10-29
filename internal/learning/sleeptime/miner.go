package sleeptime

import (
	"context"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func classifyError(errStr string) string {
	errorMatchers := []struct {
		cat      string
		patterns []string
	}{
		{"timeout", []string{"timeout", "deadline"}},
		{"canceled", []string{"cancel"}},
		{"rate_limit", []string{"rate limit", "429", "quota"}},
		{"schema_error", []string{"unmarshal", "json", "syntax", "eof"}},
		{"permission_denied", []string{"forbidden", "permission", "denied", "blocked"}},
		{"not_found", []string{"not found", "no such file"}},
		{"command_failed", []string{"exit status", "command failed"}},
	}

	lower := strings.ToLower(errStr)
	for _, m := range errorMatchers {
		for _, p := range m.patterns {
			if strings.Contains(lower, p) {
				return m.cat
			}
		}
	}
	return "execution_error"
}

func recordFailure(clusters map[string]*Weakness, purpose, errStr string) {
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
		for _, s := range w.SampleErrors {
			if s == errStr {
				return
			}
		}
		w.SampleErrors = append(w.SampleErrors, errStr)
	}
}

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
		recordFailure(clusters, purpose, errStr)
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
