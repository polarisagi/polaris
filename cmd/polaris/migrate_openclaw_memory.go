// OpenClaw 记忆数据库→polaris EventLog 映射迁移。
// 设计意图: OpenClaw 使用 SQLite 存储记忆，schema 不固定（各版本差异大），
// 通过 PRAGMA table_info 自省列名模式（content/role/timestamp/embedding/session_id）自动映射，
// 避免硬编码表结构。staging 模式将记忆写入隔离命名空间，由 M5 ConsolidationPipeline 渐进吸收，
// 防止外部低质量记忆直接污染主线 EventLog。
// 架构文档: docs/arch/M13-Interface-Scheduler.md §1.1 "外部平台迁移"
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// ─── 配置常量 ─────────────────────────────────────────────────────────────────────────

const (
	topicStaging    = "memory.openclaw.staging" // 隔离命名空间, M5 Consolidation 按需消费
	topicDirect     = "memory.openclaw"         // 直接进入主线 EventLog
	salienceDefault = 0.3                       // staging 低优先级, 待 Consolidation 处理后提升
	salienceDirect  = 0.5                       // 直接模式默认显著性
	maxRowsPerTable = 10000
	maxSmartRows    = 2000 // smart 模式经 LLM 预压缩后上限
)

// ─── 迁移入口 ──────────────────────────────────────────────────────────────────────────

type rawRow struct {
	content   string
	role      string
	ts        int64
	sessionID string
	title     string
}

func migrateMemory(dbPath string, stage bool, smart bool) error {
	polarisDB := resolvePolarisDB()

	ocDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "打开 OpenClaw 记忆数据库失败: "+dbPath, err)
	}
	defer ocDB.Close()

	tables, err := introspectTables(ocDB)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "自省 OpenClaw 数据库 schema 失败", err)
	}
	if len(tables) == 0 {
		fmt.Println("SKIP  memory db: no tables found")
		return nil
	}

	polDB, err := sql.Open("sqlite", polarisDB)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "打开 polaris 数据库失败: "+polarisDB, err)
	}
	defer polDB.Close()

	if err := verifyEventsTable(polDB); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "polaris events 表未就绪", err)
	}

	topic := topicStaging
	salience := salienceDefault
	modeLabel := "STAGING"
	if !stage {
		topic = topicDirect
		salience = salienceDirect
		modeLabel = "DIRECT"
	}
	fmt.Printf("MODE  %s (topic=%s, salience=%.1f)\n", modeLabel, topic, salience)

	if stage {
		if err := ensureStagingTable(polDB); err != nil {
			return perrors.Wrap(perrors.CodeInternal, "创建 migration_staging 表失败", err)
		}
	}

	total := 0
	ocFriendly := filepath.Base(dbPath)
	batchID := fmt.Sprintf("oc-mig-%d", time.Now().UnixMilli())

	tableSizes := make(map[string]int)
	for _, tbl := range tables {
		var n int
		ocDB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", quoteTable(tbl))).Scan(&n) //nolint:errcheck
		tableSizes[tbl] = n
	}

	for _, tbl := range tables {
		opts := migrateOpts{
			stage:    stage,
			batchID:  batchID,
			topic:    topic,
			salience: salience,
			smart:    smart,
		}

		mapped, err := mapTableWithOpts(ocDB, polDB, tbl, opts)
		if err != nil {
			fmt.Printf("WARN  table %s: %v\n", tbl, err)
			continue
		}
		if mapped > 0 {
			fmt.Printf("OK    table %s → events (%d rows)\n", tbl, mapped)
			total += mapped
		} else {
			fmt.Printf("INFO  table %s: no mappable rows (skipped)\n", tbl)
		}
	}

	if stage && total > 0 {
		recordBatch(polDB, batchID, total, ocFriendly, tableSizes)
		fmt.Println("INFO  memory batch recorded in migration_staging. Run:")
		fmt.Println("        polaris memory process-staging")
		fmt.Println("      to trigger M5 ConsolidationPipeline 将 staging 记忆渐进吸收到主线")
	}

	fmt.Printf("\nDONE  migrated %d memory records from %s to polaris.events\n", total, ocFriendly)
	return nil
}

// ─── 迁移参数 ──────────────────────────────────────────────────────────────────────────

type migrateOpts struct {
	stage    bool
	batchID  string
	topic    string
	salience float64
	smart    bool
}

// ─── Staging 元信息表 ──────────────────────────────────────────────────────────────────

func ensureStagingTable(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS migration_staging (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id    TEXT NOT NULL UNIQUE,
			total_rows  INTEGER NOT NULL,
			source_file TEXT,
			table_sizes TEXT,  -- JSON
			topic       TEXT NOT NULL DEFAULT 'memory.openclaw.staging',
			processed   INTEGER NOT NULL DEFAULT 0, -- 0=pending, 1=in_progress, 2=done
			created_at  INTEGER NOT NULL,
			processed_at INTEGER
		)`)
	return err
}

func recordBatch(db *sql.DB, batchID string, total int, source string, sizes map[string]int) {
	sizesJSON, _ := json.Marshal(sizes)
	db.Exec( //nolint:errcheck
		`INSERT OR REPLACE INTO migration_staging (batch_id, total_rows, source_file, table_sizes, topic, processed, created_at)
		 VALUES (?, ?, ?, ?, ?, 0, ?)`,
		batchID, total, source, string(sizesJSON), topicStaging, time.Now().UnixMilli(),
	)
}

// ─── 表级映射（带选项） ────────────────────────────────────────────────────────────────

func mapTableWithOpts(ocDB, polDB *sql.DB, table string, opts migrateOpts) (int, error) { //nolint:gocyclo
	cols, err := introspectColumns(ocDB, table)
	if err != nil {
		return 0, perrors.Wrap(perrors.CodeInternal, "自省列信息失败: "+table, err)
	}
	if len(cols) == 0 {
		return 0, nil
	}

	contentCol := findPattern(cols, colContent)
	roleCol := findPattern(cols, colRole)
	tsCol := findPattern(cols, colTS)
	sessionCol := findPattern(cols, colSession)
	titleCol := findPattern(cols, colTitle)

	if contentCol == nil {
		return 0, nil
	}

	var rawRows []rawRow

	selectCols := []string{quoteCol(contentCol.Name)}
	if roleCol != nil {
		selectCols = append(selectCols, quoteCol(roleCol.Name))
	}
	if tsCol != nil {
		selectCols = append(selectCols, quoteCol(tsCol.Name))
	}
	if sessionCol != nil {
		selectCols = append(selectCols, quoteCol(sessionCol.Name))
	}
	if titleCol != nil {
		selectCols = append(selectCols, quoteCol(titleCol.Name))
	}

	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL AND %s != '' ORDER BY ",
		strings.Join(selectCols, ", "), quoteTable(table),
		quoteCol(contentCol.Name), quoteCol(contentCol.Name))
	if tsCol != nil {
		query += fmt.Sprintf("%s ASC", quoteCol(tsCol.Name))
	} else {
		query += "rowid"
	}
	query += fmt.Sprintf(" LIMIT %d", maxRowsPerTable)

	ocRows, err := ocDB.Query(query)
	if err != nil {
		return 0, perrors.Wrap(perrors.CodeInternal, "查询表失败: "+table, err)
	}
	defer ocRows.Close()

	for ocRows.Next() {
		vals := make([]any, len(selectCols))
		ptrs := make([]any, len(selectCols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := ocRows.Scan(ptrs...); err != nil {
			return 0, perrors.Wrap(perrors.CodeInternal, "扫描行数据失败", err)
		}

		idx := 0
		content := readString(vals[idx])
		idx++
		role := ""
		if roleCol != nil && idx < len(vals) {
			role = readString(vals[idx])
			idx++
		}
		ts := int64(0)
		if tsCol != nil && idx < len(vals) {
			ts = readTimestamp(vals[idx])
			idx++
		}
		sessionID := ""
		if sessionCol != nil && idx < len(vals) {
			sessionID = readString(vals[idx])
			idx++
		}
		title := ""
		if titleCol != nil && idx < len(vals) {
			title = readString(vals[idx])
		}

		rawRows = append(rawRows, rawRow{
			content: content, role: role, ts: ts,
			sessionID: sessionID, title: title,
		})
	}

	if len(rawRows) == 0 {
		return 0, nil
	}

	skipSet := make(map[int]bool)
	var summaryRows []rawRow
	if opts.smart {
		skipSet, summaryRows = smartCompress(rawRows)
	}

	insertStmt, err := polDB.Prepare(`
		INSERT OR IGNORE INTO events
			(id, topic, actor, type, payload, memory_layer, salience, occurred_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, perrors.Wrap(perrors.CodeInternal, "准备 events 插入语句失败", err)
	}
	defer insertStmt.Close()

	mapped := 0
	now := time.Now().UnixMilli()
	counter := 0

	doInsert := func(r rawRow, salienceBoost float64) bool {
		ts := r.ts
		if ts == 0 {
			ts = now
		}
		counter++
		eventID := fmt.Sprintf("oc-mig-%d-%d", now, counter)

		et := "observation"
		role := strings.ToLower(r.role)
		switch role {
		case "assistant", "bot", "ai", "agent":
			et = "reflection"
		case "user", "human":
			et = "observation"
		case "system":
			et = "system"
		default:
			if role == "" && r.title != "" {
				et = "reflection"
			}
		}

		payload := map[string]any{
			"source":  "openclaw_migration",
			"table":   table,
			"content": r.content,
		}
		if r.role != "" {
			payload["role"] = r.role
		}
		if r.title != "" {
			payload["title"] = r.title
		}
		if r.sessionID != "" {
			payload["session_id"] = r.sessionID
		}

		payloadJSON, _ := json.Marshal(payload)

		actor := fmt.Sprintf("migration:openclaw/%s", opts.batchID)
		sal := opts.salience + salienceBoost
		if sal > 0.9 {
			sal = 0.9
		}

		_, err := insertStmt.Exec(eventID, opts.topic, actor, et, payloadJSON, "episodic", sal, ts, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "too long") {
				return false
			}
			fmt.Printf("WARN  insert event: %v\n", err)
			return false
		}
		return true
	}

	for _, r := range summaryRows {
		if doInsert(r, 0.1) {
			mapped++
		}
	}

	for i, r := range rawRows {
		if skipSet[i] {
			continue
		}
		if doInsert(r, 0) {
			mapped++
		}
	}

	if opts.smart {
		totalRows := len(rawRows)
		skippedRows := len(skipSet)
		summaryCount := len(summaryRows)
		if skippedRows > 0 || summaryCount > 0 {
			fmt.Printf("      smart: %d→%d rows (%d skipped, %d summaries)\n",
				totalRows, mapped, skippedRows, summaryCount)
		}
	}

	return mapped, nil
}

// ─── Smart 压缩 ────────────────────────────────────────────────────────────────────────

// smartCompress 对原始行做去重+摘要，减少低价值记忆进入主线。
// 返回 skipSet（应跳过的行索引）和 summaries（每 session 的摘要行）。
func smartCompress(rows []rawRow) (skipSet map[int]bool, summaries []rawRow) {
	skipSet = make(map[int]bool)

	bySession := make(map[string][]int)
	for i, r := range rows {
		sid := r.sessionID
		if sid == "" {
			sid = "_no_session"
		}
		bySession[sid] = append(bySession[sid], i)
	}

	for _, indices := range bySession {
		sort.Slice(indices, func(a, b int) bool {
			if rows[indices[a]].ts != rows[indices[b]].ts {
				return rows[indices[a]].ts < rows[indices[b]].ts
			}
			return indices[a] < indices[b]
		})

		// 跳过空内容（<10 字符）和重复系统消息
		filtered := indices[:0]
		for _, idx := range indices {
			content := strings.TrimSpace(rows[idx].content)
			if content == "" || len(content) < 10 {
				skipSet[idx] = true
				continue
			}
			role := strings.ToLower(rows[idx].role)
			if role == "system" && len(filtered) > 1 {
				skipSet[idx] = true
				continue
			}
			filtered = append(filtered, idx)
		}

		if len(filtered) <= 2 {
			continue
		}

		// Levenshtein 相似度 >0.9 视为重复，跳过
		prevContent := ""
		for i, idx := range filtered {
			if i == 0 {
				prevContent = rows[idx].content
				continue
			}
			if similarity(rows[idx].content, prevContent) > 0.9 {
				skipSet[idx] = true
			} else {
				prevContent = rows[idx].content
			}
		}

		summary := buildSessionSummary(rows, filtered, skipSet)
		if summary.content != "" {
			summaries = append(summaries, summary)
		}
	}

	return skipSet, summaries
}

func similarity(a, b string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	// 仅比较前 500 字符，减少计算量
	maxLen := 500
	if len(a) > maxLen {
		a = a[:maxLen]
	}
	if len(b) > maxLen {
		b = b[:maxLen]
	}
	distance := levenshteinDistance(a, b)
	maxLenAB := len(a)
	if len(b) > maxLenAB {
		maxLenAB = len(b)
	}
	if maxLenAB == 0 {
		return 1.0
	}
	return 1.0 - float64(distance)/float64(maxLenAB)
}

func levenshteinDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	n, m := len(ar), len(br)
	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for j := 0; j <= m; j++ {
		prev[j] = j
	}
	for i := 1; i <= n; i++ {
		cur[0] = i
		for j := 1; j <= m; j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[m]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

func buildSessionSummary(rows []rawRow, filtered []int, skipSet map[int]bool) rawRow {
	var parts []string //nolint:prealloc
	var minTS, maxTS int64
	var roles []string //nolint:prealloc
	first := true

	for _, idx := range filtered {
		if skipSet[idx] {
			continue
		}
		content := strings.TrimSpace(rows[idx].content)
		if content == "" {
			continue
		}
		// 每行取前 120 字符作为摘要片段
		if len(content) > 120 {
			content = content[:120] + "..."
		}
		parts = append(parts, content)
		roles = append(roles, rows[idx].role)
		if first || rows[idx].ts < minTS {
			minTS = rows[idx].ts
		}
		if first || rows[idx].ts > maxTS {
			maxTS = rows[idx].ts
		}
		first = false
	}

	if len(parts) < 3 {
		return rawRow{}
	}

	// 仅取首中尾 3 个片段，避免摘要过长
	selected := parts
	if len(selected) > 3 {
		selected = []string{parts[0], parts[len(parts)/2], parts[len(parts)-1]}
	}

	summaryContent := fmt.Sprintf("[MIGRATION SUMMARY] %s", strings.Join(selected, " | "))
	summaryRole := "compaction"
	if len(roles) > 0 {
		summaryRole = roles[len(roles)-1]
	}

	return rawRow{
		content: summaryContent,
		role:    summaryRole,
		ts:      maxTS,
		title:   fmt.Sprintf("staging_summary_%d", len(filtered)),
	}
}

// ─── Schema 自省 & 工具函数 ───────────────────────────────────────────────────────────

type columnInfo struct {
	Index   int
	Name    string
	Type    string
	NotNull bool
	Default *string
	PK      bool
	Pattern columnPattern
}

type columnPattern int

const (
	colUnknown columnPattern = iota
	colContent
	colRole
	colTS
	colEmbed
	colID
	colSession
	colTitle
)

// patternKeywords: 列名→语义模式映射。OpenClaw 各版本 schema 列名不统一，
// 通过关键词后缀匹配自动识别列用途，无需硬编码表结构。
var patternKeywords = map[columnPattern][]string{
	colContent: {"content", "text", "message", "body", "prompt", "response", "output", "memory_text"},
	colRole:    {"role", "sender", "author", "speaker", "from", "who"},
	colTS:      {"timestamp", "created_at", "created", "date", "time", "updated_at"},
	colEmbed:   {"embedding", "vector", "vec", "embed"},
	colID:      {"id", "uid", "uuid", "message_id"},
	colSession: {"session_id", "conversation_id", "chat_id", "channel", "thread_id"},
	colTitle:   {"title", "name", "subject", "topic", "summary"},
}

func introspectTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func introspectColumns(db *sql.DB, table string) ([]columnInfo, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []columnInfo
	for rows.Next() {
		var c columnInfo
		var nullable int
		var def sql.NullString
		var pk int
		if err := rows.Scan(&c.Index, &c.Name, &c.Type, &nullable, &def, &pk); err != nil {
			return nil, err
		}
		c.NotNull = nullable == 0
		if def.Valid {
			c.Default = &def.String
		}
		c.PK = pk == 1
		c.Pattern = classifyColumn(c.Name)
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func classifyColumn(name string) columnPattern {
	lower := strings.ToLower(name)
	for p, keywords := range patternKeywords {
		for _, kw := range keywords {
			if lower == kw || strings.HasSuffix(lower, "_"+kw) {
				return p
			}
		}
	}
	return colUnknown
}

func findPattern(cols []columnInfo, p columnPattern) *columnInfo {
	for i := range cols {
		if cols[i].Pattern == p {
			return &cols[i]
		}
	}
	return nil
}

func quoteTable(name string) string { return fmt.Sprintf("%q", name) }
func quoteCol(name string) string   { return fmt.Sprintf("%q", name) }

func resolvePolarisDB() string {
	dir := os.Getenv("POLARIS_DATA_DIR")
	if dir != "" {
		return filepath.Join(dir, "polaris.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".polarisagi/polaris", "polaris.db")
}

func verifyEventsTable(db *sql.DB) error {
	var name string
	return db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='events'").Scan(&name)
}

func readString(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return ""
	}
}

func readTimestamp(v any) int64 {
	if v == nil {
		return 0
	}
	switch x := v.(type) {
	case int64:
		if x > 1e12 {
			return x
		}
		if x > 1e9 {
			return x * 1000
		}
		return x * 1000
	case float64:
		return int64(x)
	case string:
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UnixMilli()
		}
		if t, err := time.Parse("2006-01-02 15:04:05", x); err == nil {
			return t.UnixMilli()
		}
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case []byte:
		return readTimestamp(string(x))
	default:
		return 0
	}
}
