package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/protocol"
)

// Test_ADR0114_TrajectoryCases 验证信息获取与自动化管线评测用例的解析与判定规则。
// 为何需要此测试：
// HE-4 要求所有工具描述与 Prompt 变更必须经过 CI 门控的评测用例覆盖；
// 针对 ADR-0114（HTTP/浏览器双通道获取、自动化管线编排），本测试验证 5 条基准轨迹用例能正确被解析，
// 并通过 TrajectoryJudge 规则判定引擎验证正向序列匹配与反向违规工具拦截。
func Test_ADR0114_TrajectoryCases(t *testing.T) {
	ctx := context.Background()
	casesPath := filepath.Join("testdata", "adr0114_trajectory_cases.json")
	data, err := os.ReadFile(casesPath)
	require.NoError(t, err, "读取 ADR-0114 评测用例文件失败")

	var cases []protocol.EvalCase
	err = json.Unmarshal(data, &cases)
	require.NoError(t, err, "反序列化 ADR-0114 评测用例失败")
	require.Len(t, cases, 5, "必须包含恰好 5 条基准用例")

	judge := NewTrajectoryJudge()

	// 用例 1: Hacker News 帖子收集与总结管线 (automation_save dry_run=true -> automation_save)
	t.Run("Case1_HNDigestPipeline", func(t *testing.T) {
		c := cases[0]
		assert.Equal(t, "adr0114_case_1_hn_digest", c.ID)
		assert.Equal(t, protocol.Level3Trajectory, c.Level)

		rules, err := ParseRules(c.Expected)
		require.NoError(t, err)
		assert.Equal(t, []string{"automation_save", "automation_save"}, rules.ExpectedToolSequence)

		// 正向轨迹：先 dry_run 试运行，再正式保存
		compliantTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{
					Name: "automation_save",
					Input: map[string]any{
						"kind":    "pipeline",
						"dry_run": true,
						"pipeline": map[string]any{
							"steps": []any{
								map[string]any{"type": "feed_fetch"},
								map[string]any{"type": "llm"},
							},
						},
					},
				},
				{
					Name: "automation_save",
					Input: map[string]any{
						"kind":    "pipeline",
						"dry_run": false,
					},
				},
			},
		}
		passed, reason := judge.Evaluate(ctx, compliantTrace, rules)
		assert.True(t, passed, "符合预期的轨迹应判定通过: %s", reason)

		// 额外断言：第一次调用参数校验
		firstCall := compliantTrace.ToolCalls[0]
		assert.Equal(t, "pipeline", firstCall.Input["kind"])
		assert.Equal(t, true, firstCall.Input["dry_run"])

		// 逆向轨迹：只调用了一次保存，未完成最终落地
		incompleteTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{Name: "automation_save", Input: map[string]any{"dry_run": true}},
			},
		}
		passed, _ = judge.Evaluate(ctx, incompleteTrace, rules)
		assert.False(t, passed, "缺失第二次保存的轨迹应判定失败")
	})

	// 用例 2: 每周检查项目 README (automation_save kind=prompt)
	t.Run("Case2_ReadmeWeeklyCheck", func(t *testing.T) {
		c := cases[1]
		assert.Equal(t, "adr0114_case_2_readme_check", c.ID)

		rules, err := ParseRules(c.Expected)
		require.NoError(t, err)
		assert.Equal(t, []string{"automation_save"}, rules.ExpectedToolSequence)

		compliantTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{
					Name: "automation_save",
					Input: map[string]any{
						"kind":   "prompt",
						"prompt": "检查 README 并提出建议",
					},
				},
			},
		}
		passed, reason := judge.Evaluate(ctx, compliantTrace, rules)
		assert.True(t, passed, reason)
		assert.Equal(t, "prompt", compliantTrace.ToolCalls[0].Input["kind"])
	})

	// 用例 3: HTTP 数据面快速阅读 (web_fetch，禁止调用任何浏览器工具)
	t.Run("Case3_WebFetchArticle", func(t *testing.T) {
		c := cases[2]
		assert.Equal(t, "adr0114_case_3_web_fetch_article", c.ID)

		rules, err := ParseRules(c.Expected)
		require.NoError(t, err)
		assert.Equal(t, []string{"web_fetch"}, rules.ExpectedToolSequence)
		assert.Contains(t, rules.ProhibitedTools, "mcp__polaris-browser-use__browser_open")

		// 正向轨迹：纯 HTTP web_fetch
		compliantTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{
					Name: "web_fetch",
					Input: map[string]any{
						"url":  "https://example.com/post",
						"mode": "markdown",
					},
				},
			},
		}
		passed, reason := judge.Evaluate(ctx, compliantTrace, rules)
		assert.True(t, passed, reason)

		// 违规轨迹：越级拉起重型浏览器
		prohibitedTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{Name: "mcp__polaris-browser-use__browser_open"},
				{Name: "web_fetch"},
			},
		}
		passed, reason = judge.Evaluate(ctx, prohibitedTrace, rules)
		assert.False(t, passed, "调用违禁浏览器工具应被拦截")
		assert.Contains(t, reason, "used prohibited tool")
	})

	// 用例 4: 社交站点结构化采集 (social_search，禁止调用 browser_snapshot)
	t.Run("Case4_ZhihuHarvest", func(t *testing.T) {
		c := cases[3]
		assert.Equal(t, "adr0114_case_4_zhihu_harvest", c.ID)

		rules, err := ParseRules(c.Expected)
		require.NoError(t, err)
		assert.Equal(t, []string{"mcp__polaris-social__social_search"}, rules.ExpectedToolSequence)

		// 正向轨迹：直接使用 social_search 配方
		compliantTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{
					Name: "mcp__polaris-social__social_search",
					Input: map[string]any{
						"site":   "zhihu",
						"action": "search",
						"params": map[string]any{"q": "AI Agent 落地"},
					},
				},
			},
		}
		passed, reason := judge.Evaluate(ctx, compliantTrace, rules)
		assert.True(t, passed, reason)

		// 违规轨迹：多余调用 browser_snapshot 浪费 token
		wastefulTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{Name: "browser_snapshot"},
				{Name: "mcp__polaris-social__social_search"},
			},
		}
		passed, reason = judge.Evaluate(ctx, wastefulTrace, rules)
		assert.False(t, passed, "调用多余快照应被拦截")
		assert.Contains(t, reason, "used prohibited tool")
	})

	// 用例 5: 定时任务查询与删除 (automation_list -> automation_delete)
	t.Run("Case5_AutomationListDelete", func(t *testing.T) {
		c := cases[4]
		assert.Equal(t, "adr0114_case_5_automation_list_delete", c.ID)

		rules, err := ParseRules(c.Expected)
		require.NoError(t, err)
		assert.Equal(t, []string{"automation_list", "automation_delete"}, rules.ExpectedToolSequence)

		compliantTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{Name: "automation_list"},
				{Name: "automation_delete", Input: map[string]any{"id": "hourly_task_123"}},
			},
		}
		passed, reason := judge.Evaluate(ctx, compliantTrace, rules)
		assert.True(t, passed, reason)

		// 违规轨迹：乱序调用（未查询直接乱猜删除）
		outOfOrderTrace := &TrajectoryTrace{
			ToolCalls: []ToolCallRecord{
				{Name: "automation_delete"},
				{Name: "automation_list"},
			},
		}
		passed, reason = judge.Evaluate(ctx, outOfOrderTrace, rules)
		assert.False(t, passed, "顺序错乱应被判定失败")
		assert.Contains(t, reason, "missing expected tool sequence or out of order")
	})
}

// Test_ADR0119_ToolSearchCases 验证"非核心任务先 tool_search 再调用"评测用例的解析与判定：
// 先 tool_search 再调用目标工具的轨迹通过；跳过 tool_search 直接调用（懒加载下该工具不在 tools 里，
// 必然是幻觉调用）或只搜索不调用的轨迹不通过。
func Test_ADR0119_ToolSearchCases(t *testing.T) {
	ctx := context.Background()
	data, err := os.ReadFile(filepath.Join("testdata", "adr0119_tool_search_cases.json"))
	require.NoError(t, err)
	var cases []protocol.EvalCase
	require.NoError(t, json.Unmarshal(data, &cases))
	require.GreaterOrEqual(t, len(cases), 10, "非核心工具用例不得少于 10 条")

	judge := NewTrajectoryJudge()
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			rules, err := ParseRules(c.Expected)
			require.NoError(t, err)
			require.Len(t, rules.ExpectedToolSequence, 2)
			require.Equal(t, "tool_search", rules.ExpectedToolSequence[0])
			target := rules.ExpectedToolSequence[1]

			ok, reason := judge.Evaluate(ctx, &TrajectoryTrace{ToolCalls: []ToolCallRecord{
				{Name: "tool_search", Input: map[string]any{"query": c.Input["search_query"]}},
				{Name: target, Input: map[string]any{}},
			}}, rules)
			assert.True(t, ok, reason)

			ok, _ = judge.Evaluate(ctx, &TrajectoryTrace{ToolCalls: []ToolCallRecord{{Name: target, Input: map[string]any{}}}}, rules)
			assert.False(t, ok, "跳过 tool_search 直接调用应判不通过")

			ok, _ = judge.Evaluate(ctx, &TrajectoryTrace{ToolCalls: []ToolCallRecord{{Name: "tool_search", Input: map[string]any{}}}}, rules)
			assert.False(t, ok, "只搜索不调用应判不通过")
		})
	}
}
