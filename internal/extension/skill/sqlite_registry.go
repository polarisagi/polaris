package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	if !strings.HasPrefix(meta.Name, types.SkillPrefix) {
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

	var oldVersion string
	err := r.db.QueryRowContext(ctx, "SELECT version FROM skills WHERE name = ?", meta.Name).Scan(&oldVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return apperr.Wrap(apperr.CodeInternal, "sqlite_registry: get old version failed", err)
	}

	isUpgrade := err == nil && oldVersion != meta.Version

	query := `
		INSERT INTO skills (
			name, version, runtime, risk_level, sandbox, capabilities, exec_mode,
			ambient_priority, trust_tier, idempotent, benchmarks, instructions, deprecated, depends_on, composes_of, plugin_id, needs_compat_check,
			description, display_name, kind, model_invocable, user_invocable, skill_dir, script_path, spec, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET
			version=excluded.version,
			runtime=excluded.runtime,
			risk_level=excluded.risk_level,
			sandbox=excluded.sandbox,
			capabilities=excluded.capabilities,
			exec_mode=excluded.exec_mode,
			ambient_priority=excluded.ambient_priority,
			trust_tier=excluded.trust_tier,
			idempotent=excluded.idempotent,
			benchmarks=excluded.benchmarks,
			instructions=excluded.instructions,
			deprecated=excluded.deprecated,
			depends_on=excluded.depends_on,
			composes_of=excluded.composes_of,
			plugin_id=excluded.plugin_id,
			needs_compat_check=excluded.needs_compat_check,
			description=excluded.description,
			display_name=excluded.display_name,
			kind=excluded.kind,
			model_invocable=excluded.model_invocable,
			user_invocable=excluded.user_invocable,
			skill_dir=excluded.skill_dir,
			script_path=excluded.script_path,
			spec=excluded.spec,
			updated_at=CURRENT_TIMESTAMP
	`
	_, err = r.db.ExecContext(ctx, query,
		meta.Name, meta.Version, meta.Runtime, meta.RiskLevel, meta.Sandbox,
		string(capsBytes), meta.ExecMode, meta.AmbientPriority, int(meta.Trust), meta.Idempotent, string(benchBytes), meta.Instructions, meta.Deprecated,
		string(dependsJSON), string(composesJSON), meta.PluginID, meta.NeedsCompatCheck,
		meta.Description, meta.DisplayName, firstNonEmptyStr(meta.Kind, "skill"), !meta.DisableModelInvocation, !meta.DisableUserInvocation,
		meta.SkillDir, meta.ScriptPath, firstNonEmptyStr(meta.Spec, "{}"),
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "sqlite_registry: insert failed", err)
	}

	if isUpgrade {
		if scanErr := r.markReverseDependenciesCompatCheck(ctx, meta.Name); scanErr != nil {
			return apperr.Wrap(apperr.CodeInternal, "sqlite_registry: reverse dependency scan failed", scanErr)
		}
	}

	return nil
}

// markReverseDependenciesCompatCheck performs a reverse BFS to mark dependent skills.
func (r *SQLiteRegistryImpl) markReverseDependenciesCompatCheck(ctx context.Context, targetSkill string) error {
	visited := make(map[string]bool)
	queue := []string{targetSkill}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if visited[cur] {
			continue
		}
		visited[cur] = true

		query := `SELECT name, depends_on, composes_of FROM skills WHERE depends_on LIKE ? OR composes_of LIKE ?`
		pattern := `%"` + cur + `"%`
		rows, err := r.db.QueryContext(ctx, query, pattern, pattern)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "sqlite_registry: 查询 skills 依赖失败", err)
		}

		// 2026-08-08：本段原先三处全静默（Scan 走 `err == nil` 分支、两处
		// `_ = json.Unmarshal`、UPDATE 走 `_, _ =`）。四条失败路径导向同一个
		// 后果——依赖方的 needs_compat_check 停在 0，升级后不再被要求重新
		// 校验兼容性，会带着不兼容的依赖继续执行。解析类失败不中断整轮 BFS
		// （单行畸形不该让整棵依赖树漏标），但必须留痕；UPDATE 失败直接上抛，
		// 调用方（line 90）已按错误处理。
		parents, err := scanDependentSkills(ctx, rows, cur)
		rows.Close()
		if err != nil {
			return err
		}

		for _, p := range parents {
			if !visited[p] && p != targetSkill {
				queue = append(queue, p)
				if _, err := r.db.ExecContext(ctx, "UPDATE skills SET needs_compat_check = 1, updated_at = CURRENT_TIMESTAMP WHERE name = ?", p); err != nil {
					return apperr.Wrap(apperr.CodeInternal, "sqlite_registry: 标记 needs_compat_check 失败", err)
				}
			}
		}
	}
	return nil
}

// scanDependentSkills 从查询结果中挑出真正依赖/组合了 cur 的技能名。
// LIKE 预筛只保证 JSON 文本里出现过该子串，仍需解出数组逐项精确比对。
//
// 解析类失败按"该项无此依赖"降级并留痕——单行畸形 JSON 不应让整棵依赖树
// 漏标；迭代类失败上抛，因为它意味着结果集被截断，漏标范围不可知。
func scanDependentSkills(ctx context.Context, rows *sql.Rows, cur string) ([]string, error) {
	var parents []string
	for rows.Next() {
		var name, dJSON, cJSON string
		if err := rows.Scan(&name, &dJSON, &cJSON); err != nil {
			slog.WarnContext(ctx, "sqlite_registry: 依赖行扫描失败，该技能本轮漏标 needs_compat_check",
				"cur", cur, "err", err)
			continue
		}
		var deps, comps []string
		if err := json.Unmarshal([]byte(dJSON), &deps); err != nil {
			slog.WarnContext(ctx, "sqlite_registry: depends_on 解析失败，按无依赖处理",
				"skill", name, "err", err)
		}
		if err := json.Unmarshal([]byte(cJSON), &comps); err != nil {
			slog.WarnContext(ctx, "sqlite_registry: composes_of 解析失败，按无组合处理",
				"skill", name, "err", err)
		}
		if slices.Contains(deps, cur) || slices.Contains(comps, cur) {
			parents = append(parents, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sqlite_registry: 依赖行迭代失败", err)
	}
	return parents, nil
}

func (r *SQLiteRegistryImpl) Get(ctx context.Context, name, version string) (*types.SkillMeta, error) {
	// LEFT JOIN extension_instances 获取 marketplace 安装路径；builtin/user 技能 install_path 为空
	query := `SELECT ` + skillColumns + ` FROM skills WHERE name = ?`
	args := []any{name}
	if version != "" {
		query += " AND version = ?"
		args = append(args, version)
	}
	meta, err := scanSkill(r.db.QueryRowContext(ctx, query, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSkillNotFound
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "sqlite_registry: get failed", err)
	}
	return meta, nil
}

func (r *SQLiteRegistryImpl) List(ctx context.Context, filter types.SkillFilter) ([]types.SkillMeta, error) {
	query := `SELECT ` + skillColumns + ` FROM skills WHERE 1=1`
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
		meta, err := scanSkill(rows)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRegistryImpl.List", err)
		}

		// 内存级二次过滤
		if filter.RiskLevelMax != "" && riskGT(meta.RiskLevel, filter.RiskLevelMax) {
			continue
		}
		if len(filter.Capabilities) > 0 && !hasCapability(meta.Capabilities, filter.Capabilities) {
			continue
		}

		result = append(result, *meta)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRegistryImpl.List: rows", err)
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

const skillColumns = `name, version, runtime, risk_level, sandbox, capabilities, exec_mode, ambient_priority,
	trust_tier, idempotent, benchmarks, instructions, deprecated, depends_on, composes_of, plugin_id, needs_compat_check,
	description, display_name, kind, model_invocable, user_invocable, skill_dir, script_path, spec`

type rowScanner interface {
	Scan(dest ...any) error
}

// scanSkill 行 → SkillMeta。ScriptPath 取自 script_path 列：此前按 extension_instances.install_path
// 拼接 "/src/skill.py"，runtime_id 正确回写后会把纯指令技能误当脚本技能执行不存在的文件。
func scanSkill(row rowScanner) (*types.SkillMeta, error) {
	var meta types.SkillMeta
	var capsRaw, benchRaw, dependsJSON, composesJSON string
	var trustInt, needsCompatCheck, modelInvocable, userInvocable int
	if err := row.Scan(
		&meta.Name, &meta.Version, &meta.Runtime, &meta.RiskLevel, &meta.Sandbox,
		&capsRaw, &meta.ExecMode, &meta.AmbientPriority, &trustInt, &meta.Idempotent, &benchRaw, &meta.Instructions, &meta.Deprecated,
		&dependsJSON, &composesJSON, &meta.PluginID, &needsCompatCheck,
		&meta.Description, &meta.DisplayName, &meta.Kind, &modelInvocable, &userInvocable, &meta.SkillDir, &meta.ScriptPath, &meta.Spec,
	); err != nil {
		return nil, err //nolint:wrapcheck // 调用方按 sql.ErrNoRows 判定后再包装
	}
	meta.Trust = types.TrustTier(trustInt)
	meta.NeedsCompatCheck = needsCompatCheck == 1
	meta.DisableModelInvocation = modelInvocable == 0
	meta.DisableUserInvocation = userInvocable == 0
	for _, f := range []struct {
		raw string
		dst any
	}{{capsRaw, &meta.Capabilities}, {benchRaw, &meta.Benchmarks}, {dependsJSON, &meta.DependsOn}, {composesJSON, &meta.ComposesOf}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			// L3：单个元数据字段损坏不影响技能主体可用性，留痕。
			slog.Warn("sqlite_registry: corrupt skill metadata column", "skill", meta.Name, "err", err)
		}
	}
	return &meta, nil
}

func firstNonEmptyStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
