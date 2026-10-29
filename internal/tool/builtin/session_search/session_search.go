// Package session_search 实现只读的跨会话对话检索工具（ADR-0120 决策六）。
package session_search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	defaultLimit  = 8
	maxLimit      = 30
	snippetRadius = 80
	// likeScanRows 是 LIKE 回退时最多扫描的最近消息条数（有界扫描，防止全表）。
	likeScanRows = 5000
	// minFTSRunes 是 trigram 索引能命中的最短词长。
	minFTSRunes = 3
)

type args struct {
	Query     string `json:"query"`
	SessionID string `json:"session_id"`
	Since     string `json:"since"`
	Limit     int    `json:"limit"`
}

// MakeSessionSearchFn 构造 session_search 执行函数。
func MakeSessionSearchFn(db protocol.SQLQuerier) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var a args
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "session_search: invalid args", err)
		}
		terms := strings.Fields(a.Query)
		if len(terms) == 0 {
			return nil, apperr.New(apperr.CodeInvalidInput, "session_search: query is required")
		}
		if db == nil {
			return nil, apperr.New(apperr.CodeInternal, "session_search: database not configured")
		}
		if a.Limit <= 0 {
			a.Limit = defaultLimit
		}
		if a.Limit > maxLimit {
			a.Limit = maxLimit
		}
		since, err := parseSince(a.Since)
		if err != nil {
			return nil, err
		}
		cur, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
		q, qargs := buildQuery(terms, a.SessionID, cur, since, a.Limit)
		rows, err := db.QueryContext(ctx, q, qargs...)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "session_search: query", err)
		}
		defer func() { _ = rows.Close() }()
		return render(rows, terms)
	}
}

func parseSince(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05Z"), nil
		}
	}
	return "", apperr.New(apperr.CodeInvalidInput, "session_search: since must be YYYY-MM-DD or RFC3339")
}

func allLongEnough(terms []string) bool {
	for _, t := range terms {
		if utf8.RuneCountInString(t) < minFTSRunes {
			return false
		}
	}
	return true
}

// ftsPhrase 把一个词转成 FTS5 短语字面量（trigram 下等价于子串匹配）。
func ftsPhrase(t string) string {
	return `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
}

func likeEscape(t string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(t)
}

// buildQuery 选择 FTS（所有词 ≥3 字符）或有界 LIKE（含 1-2 字符词，常见于中文）。
func buildQuery(terms []string, sessionID, currentID, since string, limit int) (string, []any) {
	var where []string
	var wargs []any
	switch {
	case sessionID != "":
		where = append(where, "m.session_id = ?")
		wargs = append(wargs, sessionID)
	case currentID != "":
		where = append(where, "m.session_id != ?")
		wargs = append(wargs, currentID)
	}
	if since != "" {
		where = append(where, "m.created_at >= ?")
		wargs = append(wargs, since)
	}
	extra := ""
	if len(where) > 0 {
		extra = " AND " + strings.Join(where, " AND ")
	}
	if allLongEnough(terms) {
		phr := make([]string, len(terms))
		for i, t := range terms {
			phr[i] = ftsPhrase(t)
		}
		q := `SELECT m.id, m.session_id, m.role, m.created_at, m.content
FROM messages_trigram_fts f JOIN chat_messages m ON m.id = f.rowid
WHERE messages_trigram_fts MATCH ? AND m.role IN ('user','assistant')` + extra + `
ORDER BY m.id DESC LIMIT ?`
		a := append([]any{strings.Join(phr, " AND ")}, wargs...)
		return q, append(a, limit)
	}
	var likes []string
	var largs []any
	for _, t := range terms {
		likes = append(likes, `content LIKE ? ESCAPE '\'`)
		largs = append(largs, "%"+likeEscape(t)+"%")
	}
	inner := `SELECT m.id, m.session_id, m.role, m.created_at, m.content FROM chat_messages m
WHERE m.role IN ('user','assistant')` + extra + ` ORDER BY m.id DESC LIMIT ` + fmt.Sprint(likeScanRows)
	q := `SELECT id, session_id, role, created_at, content FROM (` + inner + `) WHERE ` +
		strings.Join(likes, " AND ") + ` LIMIT ?`
	a := append(wargs, largs...)
	return q, append(a, limit)
}

func render(rows *sql.Rows, terms []string) ([]byte, error) {
	var sb strings.Builder
	n := 0
	for rows.Next() {
		var id int64
		var sid, role, at, content string
		if err := rows.Scan(&id, &sid, &role, &at, &content); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "session_search: scan", err)
		}
		n++
		fmt.Fprintf(&sb, "[session=%s message=%d %s %s] %s\n", sid, id, role, at, snippet(content, terms[0]))
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "session_search: rows", err)
	}
	if n == 0 {
		return []byte("no matches"), nil
	}
	sb.WriteString("use session_read{session_id, message_id} to read around a hit")
	return []byte(sb.String()), nil
}

// snippet 取首个匹配词前后各 snippetRadius 个字符（按 rune，大小写不敏感）。
func snippet(content, term string) string {
	rs := []rune(content)
	lower := []rune(strings.ToLower(content))
	if len(lower) != len(rs) { // 极少数字符小写后长度变化：退回原文定位
		lower = rs
	}
	pos := indexRunes(lower, []rune(strings.ToLower(term)))
	if pos < 0 {
		pos = 0
	}
	start := max(0, pos-snippetRadius)
	end := min(len(rs), pos+utf8.RuneCountInString(term)+snippetRadius)
	s := strings.Join(strings.Fields(string(rs[start:end])), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(rs) {
		s += "…"
	}
	return s
}

func indexRunes(hay, needle []rune) int {
	if len(needle) == 0 {
		return -1
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
