package sysadmin

// Budget 管理 + 系统备份/恢复。
//
// Budget:
//   GET  /v1/config/budget            → 读取月度预算
//   PUT  /v1/config/budget            → 写入月度预算（kv_store key: config:budget:monthly_usd）
//
// Backup:
//   GET  /v1/export/backup            → 导出数据快照（sessions + messages + kv_store 全量 JSONL）
//   POST /v1/import/backup            → 从 JSONL 快照恢复（幂等 upsert）

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// handleGetBudget GET /v1/config/budget
func (h *SysAdminHandler) HandleGetBudget(w http.ResponseWriter, r *http.Request) {
	monthlyUSD, err := h.BudgetRepo.GetBudget(r.Context())
	if err != nil {
		monthlyUSD = 0.0
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"monthly_usd": monthlyUSD}) //nolint:errcheck
}

// handleSetBudget PUT /v1/config/budget
// Body: {"monthly_usd": 10.0}
func (h *SysAdminHandler) HandleSetBudget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MonthlyUSD float64 `json:"monthly_usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.MonthlyUSD < 0 {
		http.Error(w, "monthly_usd must be >= 0", http.StatusBadRequest)
		return
	}

	if err := h.BudgetRepo.SetBudget(r.Context(), req.MonthlyUSD); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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

// handleExportBackup GET /v1/export/backup
//
// 以 JSONL 流式导出 chat_sessions / chat_messages / kv_store 三张核心表。
// 文件格式：每行一个 backupRecord JSON 对象，首行为元数据头。
func (h *SysAdminHandler) HandleExportBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ts := time.Now().UTC().Format("20060102T150405Z")

	w.Header().Set("Content-Type", "application/jsonl")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="polaris-backup-%s.jsonl"`, ts))

	enc := json.NewEncoder(w)

	// 元数据头
	_ = enc.Encode(map[string]any{
		"table":      "__meta__",
		"version":    "1",
		"created_at": ts,
	})

	// chat_sessions
	sessRows, err := h.DB.QueryContext(ctx, `SELECT id, title, thrashing_index, created_at, updated_at FROM chat_sessions ORDER BY id`)
	if err != nil {
		slog.Warn("backup: query chat_sessions", "err", err)
	} else {
		defer sessRows.Close()
		for sessRows.Next() {
			var id, title, createdAt, updatedAt string
			var thrashing float64
			if sessRows.Scan(&id, &title, &thrashing, &createdAt, &updatedAt) == nil {
				_ = enc.Encode(backupRecord{Table: "chat_sessions", Row: map[string]any{
					"id": id, "title": title, "thrashing_index": thrashing, "created_at": createdAt, "updated_at": updatedAt,
				}})
			}
		}
	}

	// chat_messages
	msgRows, err := h.DB.QueryContext(ctx, `SELECT id, session_id, role, content, created_at FROM chat_messages ORDER BY id`)
	if err != nil {
		slog.Warn("backup: query chat_messages", "err", err)
	} else {
		defer msgRows.Close()
		for msgRows.Next() {
			var id, sessionID, role, content, createdAt string
			if msgRows.Scan(&id, &sessionID, &role, &content, &createdAt) == nil {
				_ = enc.Encode(backupRecord{Table: "chat_messages", Row: map[string]any{
					"id": id, "session_id": sessionID, "role": role, "content": content, "created_at": createdAt,
				}})
			}
		}
	}

	// kv_store（不导出 internal runtime keys，只导出 config: 前缀）
	kvRows, err := h.DB.QueryContext(ctx, `SELECT key, value, updated_at FROM kv_store WHERE key LIKE 'config:%' ORDER BY key`)
	if err != nil {
		slog.Warn("backup: query kv_store", "err", err)
	} else {
		defer kvRows.Close()
		for kvRows.Next() {
			var key, value, updatedAt string
			if kvRows.Scan(&key, &value, &updatedAt) == nil {
				_ = enc.Encode(backupRecord{Table: "kv_store", Row: map[string]any{
					"key": key, "value": value, "updated_at": updatedAt,
				}})
			}
		}
	}
}

// handleImportBackup POST /v1/import/backup
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
		case "chat_sessions":
			id, _ := row["id"].(string)
			title, _ := row["title"].(string)
			thrashing, _ := row["thrashing_index"].(float64)
			createdAt, _ := row["created_at"].(string)
			updatedAt, _ := row["updated_at"].(string)
			if id == "" {
				skipped++
				continue
			}
			err = h.ChatRepo.RestoreSession(ctx, id, title, thrashing, createdAt, updatedAt)

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
