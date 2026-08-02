package graphrag

import "time"

// AsOfFilter 时点视图过滤器（ADR-0083 双时态知识图谱）。零值（IsZero）表示
// "当前视图"，等价于 status='active'；非零值表示"历史回放视图"，按有效时间
// 区间过滤 semantic_relations / semantic_entities。
//
// 时点语义：有效于 t ⟺ (valid_from IS NULL OR valid_from <= t)
//
//	AND (valid_until IS NULL OR valid_until > t)
type AsOfFilter struct {
	At time.Time
}

// IsZero 报告过滤器是否为"当前视图"（未指定时点）。
func (f AsOfFilter) IsZero() bool {
	return f.At.IsZero()
}

// SQLWhere 返回可拼接到 semantic_relations / semantic_entities 查询的 WHERE
// 片段（不含前导 " AND "，调用方自行拼接）与对应参数。alias 为表别名前缀
// （如 "r"/"e"），空字符串表示不加前缀。
//
// 空过滤器返回 status='active' 快路径（走 idx_semantic_rel_status /
// idx_semantic_ent_status），与改造前查询计划一致——这是本次改造的兼容性底线。
func (f AsOfFilter) SQLWhere(alias string) (string, []any) {
	prefix := alias
	if prefix != "" {
		prefix += "."
	}
	if f.IsZero() {
		return prefix + "status = 'active'", nil
	}
	t := f.At.UnixMilli()
	where := "(" + prefix + "valid_from IS NULL OR " + prefix + "valid_from <= ?)" +
		" AND (" + prefix + "valid_until IS NULL OR " + prefix + "valid_until > ?)"
	return where, []any{t, t}
}
