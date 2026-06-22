package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteRegistryImpl 实现了持久化的技能注册表，基于 SQLite。
// 并发写入通过 SQLite 事务隔离保证。
type SQLiteRegistryImpl struct {
	db protocol.SQLQuerier
}

func NewSQLiteRegistry(db protocol.SQLQuerier) *SQLiteRegistryImpl {
	return &SQLiteRegistryImpl{db: db}
}

var _ protocol.SkillRegistry = (*SQLiteRegistryImpl)(nil)

// Register 插入或更新技能元数据。
func (r *SQLiteRegistryImpl) Register(ctx context.Context, meta types.SkillMeta) error {
	if meta.Trust < types.TrustLocal {
		return errCosignVerifyFailed
	}
	if !strings.HasPrefix(meta.Name, "skill:") {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("skill name error: got %s", meta.Name), errInvalidSkillName)
	}

	capsBytes, _ := json.Marshal(meta.Capabilities)
	benchBytes, _ := json.Marshal(meta.Benchmarks)

	dependsJSON, _ := json.Marshal(meta.DependsOn)
	composesJSON, _ := json.Marshal(meta.ComposesOf)

	allDeps := append(meta.DependsOn, meta.ComposesOf...)
	if err := r.detectSkillCycle(ctx, meta.Name, allDeps); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "skill dependency cycle detected", err)
	}

	query := `
		INSERT INTO skills (
			name, version, runtime, risk_level, sandbox, capabilities, exec_mode,
			trust_tier, idempotent, benchmarks, instructions, deprecated, depends_on, composes_of, plugin_id, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
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
			depends_on=excluded.depends_on,
			composes_of=excluded.composes_of,
			plugin_id=excluded.plugin_id,
			updated_at=CURRENT_TIMESTAMP
	`
	_, err := r.db.ExecContext(ctx, query,
		meta.Name, meta.Version, meta.Runtime, meta.RiskLevel, meta.Sandbox,
		string(capsBytes), meta.ExecMode, int(meta.Trust), meta.Idempotent, string(benchBytes), meta.Instructions, meta.Deprecated,
		string(dependsJSON), string(composesJSON), meta.PluginID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "sqlite_registry: insert failed", err)
	}

	return nil
}

func (r *SQLiteRegistryImpl) Get(ctx context.Context, name, version string) (*types.SkillMeta, error) {
	// LEFT JOIN extension_instances 获取 marketplace 安装路径；builtin/user 技能 install_path 为空
	query := `
		SELECT s.name, s.version, s.runtime, s.risk_level, s.sandbox, s.capabilities, s.exec_mode,
		       s.trust_tier, s.idempotent, s.benchmarks, s.instructions, s.deprecated,
		       s.depends_on, s.composes_of, s.plugin_id, COALESCE(ei.install_path, '')
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

	var meta types.SkillMeta
	var capsRaw, benchRaw, dependsJSON, composesJSON, installPath string
	var trustInt int
	err := row.Scan(
		&meta.Name, &meta.Version, &meta.Runtime, &meta.RiskLevel, &meta.Sandbox,
		&capsRaw, &meta.ExecMode, &trustInt, &meta.Idempotent, &benchRaw, &meta.Instructions, &meta.Deprecated,
		&dependsJSON, &composesJSON, &meta.PluginID, &installPath,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSkillNotFound
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "sqlite_registry: get failed", err)
	}
	meta.Trust = types.TrustTier(trustInt)
	if installPath != "" {
		meta.ScriptPath = installPath + "/src/index.ts"
	}

	json.Unmarshal([]byte(capsRaw), &meta.Capabilities)    //nolint:errcheck
	json.Unmarshal([]byte(benchRaw), &meta.Benchmarks)     //nolint:errcheck
	json.Unmarshal([]byte(dependsJSON), &meta.DependsOn)   //nolint:errcheck
	json.Unmarshal([]byte(composesJSON), &meta.ComposesOf) //nolint:errcheck

	return &meta, nil
}

func (r *SQLiteRegistryImpl) List(ctx context.Context, filter types.SkillFilter) ([]types.SkillMeta, error) {
	query := `
		SELECT name, version, runtime, risk_level, sandbox, capabilities, exec_mode,
		       trust_tier, idempotent, benchmarks, instructions, deprecated, depends_on, composes_of, plugin_id
		FROM skills WHERE 1=1
	`
	var args []any

	if !filter.IncludeDeprecated {
		query += " AND deprecated = 0"
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sqlite_registry: list query failed", err)
	}
	defer rows.Close()

	var result []types.SkillMeta
	for rows.Next() {
		var meta types.SkillMeta
		var capsRaw, benchRaw, dependsJSON, composesJSON string
		var trustInt int
		if err := rows.Scan(
			&meta.Name, &meta.Version, &meta.Runtime, &meta.RiskLevel, &meta.Sandbox,
			&capsRaw, &meta.ExecMode, &trustInt, &meta.Idempotent, &benchRaw, &meta.Instructions, &meta.Deprecated,
			&dependsJSON, &composesJSON, &meta.PluginID,
		); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRegistryImpl.List", err)
		}
		meta.Trust = types.TrustTier(trustInt)
		json.Unmarshal([]byte(capsRaw), &meta.Capabilities)    //nolint:errcheck
		json.Unmarshal([]byte(benchRaw), &meta.Benchmarks)     //nolint:errcheck
		json.Unmarshal([]byte(dependsJSON), &meta.DependsOn)   //nolint:errcheck
		json.Unmarshal([]byte(composesJSON), &meta.ComposesOf) //nolint:errcheck

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
		return apperr.Wrap(apperr.CodeInternal, "sqlite_registry: deprecate failed", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return errSkillNotFound
	}
	return nil
}

// detectSkillCycle 对 DependsOn ∪ ComposesOf 做 BFS 环检测。
// 若从 deps 出发可达 skillName，则存在循环依赖，返回非 nil error。
// 图中不存在 skillName 的邻居时视为叶节点（依赖已满足或尚未安装）。
func (r *SQLiteRegistryImpl) detectSkillCycle(ctx context.Context, skillName string, deps []string) error {
	visited := make(map[string]bool)
	queue := make([]string, len(deps))
	copy(queue, deps)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == skillName {
			return apperr.New(apperr.CodeInternal,
				fmt.Sprintf("cyclic skill dependency: %s → … → %s", skillName, skillName))
		}
		if visited[cur] {
			continue
		}
		visited[cur] = true
		// 读取当前节点的依赖
		var dJSON, cJSON string
		err := r.db.QueryRowContext(ctx,
			`SELECT depends_on, composes_of FROM skills WHERE name = ?`, cur,
		).Scan(&dJSON, &cJSON)
		if err != nil {
			continue // 节点不存在 → 叶节点，继续
		}
		var curDeps, curCompose []string
		json.Unmarshal([]byte(dJSON), &curDeps)    //nolint:errcheck
		json.Unmarshal([]byte(cJSON), &curCompose) //nolint:errcheck
		queue = append(queue, curDeps...)
		queue = append(queue, curCompose...)
	}
	return nil
}
