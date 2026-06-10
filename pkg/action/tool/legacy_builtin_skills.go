package tool

import (
	"context"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/pkg/action"
)

// getLegacyBuiltinDefs 返回过渡期保留的遗留工具定义。
//
// 处置策略（见 docs/ 分析）：
//   - 已删除（11个）：file_search（=grep）、api_call（≈fetch_url）、
//     json_parse / json_format / regex_match / markdown_render（bash 覆盖）、
//     text_summarize / text_translate / code_gen / code_review / text_extract（LLM 依赖，属 swarm 层）
//   - 已迁移为真实实现（3个）：git_diff / git_commit / template_render → git_text_tools.go
//   - 保留但明确返回 ErrNotImplemented（1个）：data_query（scope 待定：用户外部 SQLite 路径约束未明确）
func getLegacyBuiltinDefs() []struct {
	name string
	fn   action.InProcessFn
} {
	return []struct {
		name string
		fn   action.InProcessFn
	}{
		// data_query: 有实现价值（用户外部 SQLite 只读查询），但 database 路径约束和 SELECT
		// 白名单尚未定义。返回明确错误防止调用方误判为成功。
		// TODO: 实现时使用 modernc.org/sqlite（ADR-0003）+ allowedPaths 约束 + SELECT-only 校验
		{"data_query", func(_ context.Context, _ []byte) ([]byte, error) {
			return nil, perrors.New(perrors.CodeInternal,
				"data_query: not yet implemented; use bash with sqlite3 as interim")
		}},
	}
}
