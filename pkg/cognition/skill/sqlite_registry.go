package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// SQLiteRegistryImpl 实现了持久化的技能注册表，基于 SQLite。
// 并发写入通过 SQLite 事务隔离保证。
type SQLiteRegistryImpl struct {
	db *sql.DB
}

func NewSQLiteRegistry(db *sql.DB) *SQLiteRegistryImpl {
	return &SQLiteRegistryImpl{db: db}
}

var _ protocol.SkillRegistry = (*SQLiteRegistryImpl)(nil)

// Register 插入或更新技能元数据。
func (r *SQLiteRegistryImpl) Register(ctx context.Context, meta protocol.SkillMeta) error {
	if meta.Trust < protocol.TrustLocal {
		return errCosignVerifyFailed
	}
	if !strings.HasPrefix(meta.Name, "skill:") {
		return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("skill name error: got %s", meta.Name), errInvalidSkillName)
	}

	capsBytes, _ := json.Marshal(meta.Capabilities)
	benchBytes, _ := json.Marshal(meta.Benchmarks)

	query := `
		INSERT INTO skills (
			name, version, runtime, risk_level, sandbox, capabilities, exec_mode,
			trust_tier, idempotent, benchmarks, instructions, deprecated, plugin_id, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET
			version=excluded.version,
			runtime=excluded.runtime,
			risk_level=excluded.risk_level,
			sandbox=excluded.sandbox,
			capabilities=excluded.capabilities,
			exec_mode=excluded.exec_mode,
			trust_tier=excluded.trust_tier,
			idempotent=excluded.idempotent,
			benchmarks=excluded.benchmarks,
			instructions=excluded.instructions,
			deprecated=excluded.deprecated,
			plugin_id=excluded.plugin_id,
			updated_at=CURRENT_TIMESTAMP
	`
	_, err := r.db.ExecContext(ctx, query,
		meta.Name, meta.Version, meta.Runtime, meta.RiskLevel, meta.Sandbox,
		string(capsBytes), meta.ExecMode, int(meta.Trust), meta.Idempotent, string(benchBytes), meta.Instructions, meta.Deprecated,
		meta.PluginID,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "sqlite_registry: insert failed", err)
	}

	return nil
}

func (r *SQLiteRegistryImpl) Get(ctx context.Context, name, version string) (*protocol.SkillMeta, error) {
	// LEFT JOIN extension_instances 获取 marketplace 安装路径；builtin/user 技能 install_path 为空
	query := `
		SELECT s.name, s.version, s.runtime, s.risk_level, s.sandbox, s.capabilities, s.exec_mode,
		       s.trust_tier, s.idempotent, s.benchmarks, s.instructions, s.deprecated,
		       s.plugin_id, COALESCE(ei.install_path, '')
		FROM skills s
		LEFT JOIN extension_instances ei ON ei.runtime_id = s.name AND ei.ext_type = 'skill'
		WHERE s.name = ?
	`
	args := []any{name}
	if version != "" {
		query += " AND s.version = ?"
		args = append(args, version)
	}

	row := r.db.QueryRowContext(ctx, query, args...)

	var meta protocol.SkillMeta
	var capsRaw, benchRaw, installPath string
	var trustInt int
	err := row.Scan(
		&meta.Name, &meta.Version, &meta.Runtime, &meta.RiskLevel, &meta.Sandbox,
		&capsRaw, &meta.ExecMode, &trustInt, &meta.Idempotent, &benchRaw, &meta.Instructions, &meta.Deprecated,
		&meta.PluginID, &installPath,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSkillNotFound
		}
		return nil, perrors.Wrap(perrors.CodeInternal, "sqlite_registry: get failed", err)
	}
	meta.Trust = protocol.TrustTier(trustInt)
	if installPath != "" {
		meta.WasmPath = installPath + "/impl.wasm"
	}

	json.Unmarshal([]byte(capsRaw), &meta.Capabilities) //nolint:errcheck
	json.Unmarshal([]byte(benchRaw), &meta.Benchmarks)  //nolint:errcheck

	return &meta, nil
}

func (r *SQLiteRegistryImpl) List(ctx context.Context, filter protocol.SkillFilter) ([]protocol.SkillMeta, error) {
	query := `
		SELECT name, version, runtime, risk_level, sandbox, capabilities, exec_mode,
		       trust_tier, idempotent, benchmarks, instructions, deprecated, plugin_id
		FROM skills WHERE 1=1
	`
	var args []any

	if !filter.IncludeDeprecated {
		query += " AND deprecated = 0"
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "sqlite_registry: list query failed", err)
	}
	defer rows.Close()

	var result []protocol.SkillMeta
	for rows.Next() {
		var meta protocol.SkillMeta
		var capsRaw, benchRaw string
		var trustInt int
		if err := rows.Scan(
			&meta.Name, &meta.Version, &meta.Runtime, &meta.RiskLevel, &meta.Sandbox,
			&capsRaw, &meta.ExecMode, &trustInt, &meta.Idempotent, &benchRaw, &meta.Instructions, &meta.Deprecated,
			&meta.PluginID,
		); err != nil {
			return nil, err
		}
		meta.Trust = protocol.TrustTier(trustInt)
		json.Unmarshal([]byte(capsRaw), &meta.Capabilities) //nolint:errcheck
		json.Unmarshal([]byte(benchRaw), &meta.Benchmarks)  //nolint:errcheck

		// 内存级二次过滤
		if filter.RiskLevelMax != "" && riskGT(meta.RiskLevel, filter.RiskLevelMax) {
			continue
		}
		if len(filter.Capabilities) > 0 && !hasCapability(meta.Capabilities, filter.Capabilities) {
			continue
		}

		result = append(result, meta)
	}
	return result, nil
}

func (r *SQLiteRegistryImpl) Deprecate(ctx context.Context, name, version string, reason string) error {
	query := "UPDATE skills SET deprecated = 1, updated_at = CURRENT_TIMESTAMP WHERE name = ?"
	args := []any{name}
	if version != "" {
		query += " AND version = ?"
		args = append(args, version)
	}
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "sqlite_registry: deprecate failed", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return errSkillNotFound
	}
	return nil
}
