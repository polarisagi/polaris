package sysadmin

// Budget 管理 + 系统备份/恢复。
//
// Budget:
//   GET  /v1/config/budget            → 读取月度预算
//   PUT  /v1/config/budget            → 写入月度预算（kv_store key: config:budget:monthly_usd）
//
// Backup:
//   GET  /v1/export/backup            → 导出数据快照（projects + sessions + messages + kv_store 全量 JSONL）
//   POST /v1/import/backup            → 从 JSONL 快照恢复（幂等 upsert）

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/types"
)

// HandleGetBudget GET /v1/config/budget
func (h *SysAdminHandler) HandleGetBudget(w http.ResponseWriter, r *http.Request) {
	monthlyUSD, err := h.BudgetRepo.GetBudget(r.Context())
	if err != nil {
		monthlyUSD = 0.0
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"monthly_usd": monthlyUSD}) //nolint:errcheck
}

// HandleSetBudget PUT /v1/config/budget
// Body: {"monthly_usd": 10.0}
func (h *SysAdminHandler) HandleSetBudget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MonthlyUSD float64 `json:"monthly_usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if req.MonthlyUSD < 0 {
		http.Error(w, "monthly_usd must be >= 0", http.StatusBadRequest)
		return
	}

	if err := h.BudgetRepo.SetBudget(r.Context(), req.MonthlyUSD); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	// 2026-07-04 审计修复（附录·任务11）：持久化后同步热更新运行中的 Agent，
	// 否则新预算上限要等下次进程重启才对 Cedar budget_cap 生效。
	if h.Agent != nil {
		h.Agent.SetMonthlyBudgetUSD(req.MonthlyUSD)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"monthly_usd": req.MonthlyUSD, "status": "ok"}) //nolint:errcheck
}

// ─── 备份 / 恢复 ─────────────────────────────────────────────────────────────

// backupRecord 备份文件中的单条记录。
type backupRecord struct {
	Table string         `json:"table"`
	Row   map[string]any `json:"row"`
}

// HandleExportBackup GET /v1/export/backup
//
// 以 JSONL 流式导出 projects / chat_sessions / chat_messages / kv_store 四张核心表。
// projects 必须先于 chat_sessions 输出：恢复时逐行 upsert，会话的 project_id
// 外键要求所属项目先存在（否则落默认项目，见 RestoreSession）。
// 文件格式：每行一个 backupRecord JSON 对象，首行为元数据头。
func (h *SysAdminHandler) HandleExportBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ts := time.Now().UTC().Format("20060102T150405Z")

	w.Header().Set("Content-Type", "application/jsonl")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="polaris-backup-%s.jsonl"`, ts))

	enc := json.NewEncoder(w)
	// 编码失败 = 客户端断开或写出错；继续写只会逐行重复失败，记一次并停止输出。
	var encErr error
	emit := func(v any) {
		if encErr != nil {
			return
		}
		if encErr = enc.Encode(v); encErr != nil {
			slog.Warn("backup: write failed, aborting export", "err", encErr)
		}
	}

	// 元数据头
	emit(map[string]any{
		"table":      "__meta__",
		"version":    "1",
		"created_at": ts,
	})

	// projects（ADR-0097）。trusted 不导出：信任是本机显式授予，恢复时一律落 false。
	h.exportTable(ctx, emit, "projects",
		`SELECT id, name, root_path, instructions, archived, created_at, updated_at FROM projects ORDER BY id`,
		func(rows *sql.Rows) (map[string]any, error) {
			var id, name, root, instructions, createdAt, updatedAt string
			var archived int
			if err := rows.Scan(&id, &name, &root, &instructions, &archived, &createdAt, &updatedAt); err != nil {
				return nil, err //nolint:wrapcheck // 仅由 exportTable 记录日志
			}
			return map[string]any{
				"id": id, "name": name, "root_path": root, "instructions": instructions,
				"archived": archived == 1, "created_at": createdAt, "updated_at": updatedAt,
			}, nil
		})

	h.exportTable(ctx, emit, "chat_sessions",
		`SELECT id, title, project_id, thrashing_index, created_at, updated_at FROM chat_sessions ORDER BY id`,
		func(rows *sql.Rows) (map[string]any, error) {
			var id, title, projectID, createdAt, updatedAt string
			var thrashing float64
			if err := rows.Scan(&id, &title, &projectID, &thrashing, &createdAt, &updatedAt); err != nil {
				return nil, err //nolint:wrapcheck // 仅由 exportTable 记录日志
			}
			return map[string]any{
				"id": id, "title": title, "project_id": projectID, "thrashing_index": thrashing,
				"created_at": createdAt, "updated_at": updatedAt,
			}, nil
		})

	h.exportTable(ctx, emit, "chat_messages",
		`SELECT id, session_id, role, content, created_at FROM chat_messages ORDER BY id`,
		func(rows *sql.Rows) (map[string]any, error) {
			var id, sessionID, role, content, createdAt string
			if err := rows.Scan(&id, &sessionID, &role, &content, &createdAt); err != nil {
				return nil, err //nolint:wrapcheck // 仅由 exportTable 记录日志
			}
			return map[string]any{
				"id": id, "session_id": sessionID, "role": role, "content": content, "created_at": createdAt,
			}, nil
		})

	// kv_store（不导出 internal runtime keys，只导出 config: 前缀）
	h.exportTable(ctx, emit, "kv_store",
		`SELECT key, value, updated_at FROM kv_store WHERE key LIKE 'config:%' ORDER BY key`,
		func(rows *sql.Rows) (map[string]any, error) {
			var key, value, updatedAt string
			if err := rows.Scan(&key, &value, &updatedAt); err != nil {
				return nil, err //nolint:wrapcheck // 仅由 exportTable 记录日志
			}
			return map[string]any{"key": key, "value": value, "updated_at": updatedAt}, nil
		})
}

// exportTable 查询一张表并逐行输出为 backupRecord。查询失败只记日志跳过该表（与拆分前
// 行为一致：备份尽力而为，一张表失败不拖垮其余表）；单行扫描失败跳过该行并记日志。
func (h *SysAdminHandler) exportTable(ctx context.Context, emit func(any), table, query string,
	row func(*sql.Rows) (map[string]any, error)) {
	rows, err := h.DB.QueryContext(ctx, query)
	if err != nil {
		slog.Warn("backup: query failed", "table", table, "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		m, scanErr := row(rows)
		if scanErr != nil {
			slog.Warn("backup: scan row failed", "table", table, "err", scanErr)
			continue
		}
		emit(backupRecord{Table: table, Row: m})
	}
	// 迭代中途出错会表现为备份静默少行，至少留下痕迹（F-7）。
	if err := rows.Err(); err != nil {
		slog.Warn("backup: iterate failed", "table", table, "err", err)
	}
}

// HandleImportBackup POST /v1/import/backup
//
// 接收 JSONL 格式备份文件（Content-Type: application/jsonl 或 text/plain），
// 幂等 upsert 所有记录。
func (h *SysAdminHandler) HandleImportBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dec := json.NewDecoder(r.Body)
	inserted := 0
	skipped := 0

	for {
		var rec backupRecord
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if rec.Table == "__meta__" {
			continue
		}
		row := rec.Row

		var err error
		switch rec.Table {
		case "projects":
			if h.ProjectRepo == nil {
				skipped++
				continue
			}
			err = h.ProjectRepo.RestoreProject(ctx, projectFromBackup(row))

		case "chat_sessions":
			id, _ := row["id"].(string)
			title, _ := row["title"].(string)
			projectID, _ := row["project_id"].(string) // 旧备份无此字段 → 空 → 默认项目
			thrashing, _ := row["thrashing_index"].(float64)
			createdAt, _ := row["created_at"].(string)
			updatedAt, _ := row["updated_at"].(string)
			if id == "" {
				skipped++
				continue
			}
			err = h.ChatRepo.RestoreSession(ctx, id, title, projectID, thrashing, createdAt, updatedAt)

		case "chat_messages":
			id, _ := row["id"].(string)
			sessionID, _ := row["session_id"].(string)
			role, _ := row["role"].(string)
			content, _ := row["content"].(string)
			createdAt, _ := row["created_at"].(string)
			if id == "" || sessionID == "" {
				skipped++
				continue
			}
			err = h.ChatRepo.RestoreMessage(ctx, id, sessionID, role, content, createdAt)

		case "kv_store":
			key, _ := row["key"].(string)
			value, _ := row["value"].(string)
			updatedAt, _ := row["updated_at"].(string)
			if !strings.HasPrefix(key, "config:") {
				skipped++
				continue
			}
			err = h.SystemRepo.RestoreKV(ctx, key, value, updatedAt)

		default:
			skipped++
			continue
		}

		if err != nil {
			slog.Warn("import: upsert failed", "table", rec.Table, "err", err)
			skipped++
		} else {
			inserted++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"status":   "ok",
		"inserted": inserted,
		"skipped":  skipped,
	})
}

// projectFromBackup 备份行 → ProjectRow。Trusted 恒为零值：信任不随备份迁移（ADR-0097 决策二）。
func projectFromBackup(row map[string]any) types.ProjectRow {
	str := func(k string) string { v, _ := row[k].(string); return v }
	archived, _ := row["archived"].(bool)
	return types.ProjectRow{
		ID: str("id"), Name: str("name"), RootPath: str("root_path"), Instructions: str("instructions"),
		Archived: archived, CreatedAt: str("created_at"), UpdatedAt: str("updated_at"),
	}
}
