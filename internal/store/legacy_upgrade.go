package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/pricing"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 上线前阶段（CLAUDE.md §当前阶段）的 DDL 策略是直接改建表文件、开发库删除重建。
// 但用户的开发库里有不可丢的会话与记忆，不能删库：已应用过的 013/039 不会再执行，
// 新增的列与表就不会出现。本文件在迁移之后做**幂等的补齐**，让存量库与"全新建库"得到
// 同一份结构（legacy_upgrade_test.go 逐列比对两者，防止这里的清单与 SSoT 漂移）。
// 它不是 schema/ 下的补丁文件，也不改变"建表文件即 SSoT"的约定：列定义只在这里复述
// 一次 ALTER，对象（表/索引）一律从内嵌的建表文件里按名摘取，不另写第二份 DDL。

// legacyColumns 为存量库补齐的列。定义须与建表文件一致。
func legacyColumns() []struct{ table, column, def string } {
	return []struct{ table, column, def string }{
		{"llm_calls", "turn_id", "TEXT NOT NULL DEFAULT ''"},
		{"chat_messages", "turn_id", "TEXT NOT NULL DEFAULT ''"},
		{"chat_messages", "usage", "TEXT NOT NULL DEFAULT ''"},
		{"automations", "project_id", "TEXT NOT NULL DEFAULT 'default'"},
	}
}

// legacyObjects 为存量库补齐的表/索引：file 前缀 + 对象名，从建表文件摘取 CREATE 语句执行。
func legacyObjects() []struct{ filePrefix, object string } {
	return []struct{ filePrefix, object string }{
		{"013_", "chat_turn_steps"},
		{"013_", "idx_chat_turn_steps_session"},
		{"039_", "idx_llm_calls_turn"},
	}
}

// upgradeLegacySchema 幂等补齐存量库的列与对象。必须在 runMigrations 之后调用。
func (s *SQLiteStore) upgradeLegacySchema() error {
	if err := ensureColumns(s.db); err != nil {
		return err
	}
	if s.schemaFS == nil {
		return nil
	}
	for _, o := range legacyObjects() {
		stmt, err := findCreateStatement(s.schemaFS, o.filePrefix, o.object)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(stmt); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "legacy upgrade: create "+o.object, err)
		}
	}
	return nil
}

func ensureColumns(db *sql.DB) error {
	for _, c := range legacyColumns() {
		has, tableExists, err := columnExists(db, c.table, c.column)
		if err != nil {
			return err
		}
		if !tableExists || has {
			continue // 表还不存在（测试里的精简库）或列已在
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.def)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "legacy upgrade: add column "+c.table+"."+c.column, err)
		}
		slog.Info("store: 存量库补齐列", "table", c.table, "column", c.column)
	}
	return nil
}

func columnExists(db *sql.DB, table, column string) (has, tableExists bool, err error) {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, false, apperr.Wrap(apperr.CodeInternal, "legacy upgrade: table_info "+table, err)
	}
	defer rows.Close()
	for rows.Next() {
		tableExists = true
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, false, apperr.Wrap(apperr.CodeInternal, "legacy upgrade: scan table_info", err)
		}
		if name == column {
			has = true
		}
	}
	return has, tableExists, rows.Err() //nolint:wrapcheck // 迭代错误原样返回
}

// findCreateStatement 在以 filePrefix 开头的建表文件里找创建 object 的那条 CREATE 语句。
// 按 ";" 切分后剥掉每段开头的注释行再判断，故目标语句前的注释里不得出现分号。
func findCreateStatement(fsys fs.FS, filePrefix, object string) (string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "legacy upgrade: read schema dir", err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), filePrefix) || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "legacy upgrade: read "+e.Name(), err)
		}
		for _, part := range strings.Split(string(data), ";") {
			stmt := stripLeadingComments(part)
			up := strings.ToUpper(stmt)
			if !strings.HasPrefix(up, "CREATE ") {
				continue
			}
			// 对象名紧跟 "IF NOT EXISTS" 之后。
			if i := strings.Index(up, "IF NOT EXISTS"); i >= 0 {
				rest := strings.Fields(stmt[i+len("IF NOT EXISTS"):])
				if len(rest) > 0 && strings.TrimRight(strings.SplitN(rest[0], "(", 2)[0], " ") == object {
					return stmt, nil
				}
			}
		}
	}
	return "", apperr.New(apperr.CodeInternal, "legacy upgrade: 建表文件中找不到对象 "+filePrefix+"* / "+object)
}

func stripLeadingComments(s string) string {
	lines := strings.Split(s, "\n")
	i := 0
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "--") {
			i++
			continue
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines[i:], "\n"))
}

// ── llm_calls 历史费用重算（一次性数据迁移）─────────────────────────────────

const (
	repriceMarkerKey = "data_migration.llm_cost_reprice.v1"
	repriceBackupDDL = `CREATE TABLE IF NOT EXISTS llm_calls_cost_backup (
		id           TEXT PRIMARY KEY,
		cost_usd_old REAL NOT NULL,
		repriced_at  INTEGER NOT NULL
	)`
)

// RepriceLLMCalls 按价目目录（internal/llm/pricing，USD/百万 token）重算历史 llm_calls.cost_usd。
//
// 背景：旧版费率字段名为 Per1K 而填的是每百万价，且 openai_compat 适配器对 DeepSeek 流量套用
// gpt-4o-mini 价，历史行金额整体错 1000 倍量级（一次 1.2 万 token 记约 2 美元）。
//   - 只重算 model_id 命中目录的行；目录外模型的历史价无从核实，保持原值。
//   - 可逆：旧值写入 llm_calls_cost_backup（INSERT OR IGNORE，保留最早的原值），RevertLLMCostReprice 还原。
//   - 幂等：完成后在 sys_config 写标记，之后每次启动直接跳过。
func RepriceLLMCalls(db *sql.DB, nowMs int64) (updated int64, err error) {
	var done string
	if qerr := db.QueryRow("SELECT value FROM sys_config WHERE key = ?", repriceMarkerKey).Scan(&done); qerr == nil && done != "" {
		return 0, nil
	}
	if has, exists, _ := columnExists(db, "llm_calls", "cost_usd"); !exists || !has {
		return 0, nil // 精简测试库
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice: begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(repriceBackupDDL); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice: backup table", err)
	}
	models, err := distinctModelIDs(tx)
	if err != nil {
		return 0, err
	}
	for _, m := range models {
		p, ok := pricing.Lookup(m)
		if !ok {
			continue
		}
		hit := p.CacheHit
		if hit <= 0 {
			hit = p.Input
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO llm_calls_cost_backup(id, cost_usd_old, repriced_at)
			SELECT id, cost_usd, ? FROM llm_calls WHERE model_id = ?`, nowMs, m); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "reprice: backup rows", err)
		}
		res, uerr := tx.Exec(`UPDATE llm_calls SET cost_usd =
			((input_tokens - MIN(cache_hit_tokens, input_tokens)) * ? + MIN(cache_hit_tokens, input_tokens) * ? + output_tokens * ?) / 1000000.0
			WHERE model_id = ?`, p.Input, hit, p.Output, m)
		if uerr != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "reprice: update", uerr)
		}
		n, _ := res.RowsAffected()
		updated += n
	}
	if _, err = tx.Exec(`INSERT INTO sys_config(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, repriceMarkerKey, "done"); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice: marker", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice: commit", err)
	}
	return updated, nil
}

// RevertLLMCostReprice 把 RepriceLLMCalls 改过的行还原为备份里的旧值，并清除完成标记。
func RevertLLMCostReprice(db *sql.DB) (restored int64, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice revert: begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE llm_calls SET cost_usd =
		(SELECT cost_usd_old FROM llm_calls_cost_backup b WHERE b.id = llm_calls.id)
		WHERE id IN (SELECT id FROM llm_calls_cost_backup)`)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice revert: restore", err)
	}
	restored, _ = res.RowsAffected()
	if _, err = tx.Exec(`DELETE FROM sys_config WHERE key = ?`, repriceMarkerKey); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice revert: marker", err)
	}
	if _, err = tx.Exec(`DELETE FROM llm_calls_cost_backup`); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice revert: clear backup", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "reprice revert: commit", err)
	}
	return restored, nil
}

// distinctModelIDs 列出 llm_calls 中出现过的全部 model_id。
func distinctModelIDs(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query("SELECT DISTINCT model_id FROM llm_calls")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "reprice: models", err)
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "reprice: scan", err)
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "reprice: iterate models", err)
	}
	return models, nil
}
