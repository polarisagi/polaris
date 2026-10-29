package catalog

// 工具懒加载（ADR-0119）：常驻核心集 + 其余工具只在系统提示里留一行索引、由 tool_search 激活。
//
// 为什么不再依赖 Embedder：用户机器常常没有 Embedder，此前懒加载要求"工具数>阈值 且 配了 Embedder"，
// 于是 56 个工具的完整定义（约占首轮输入的 75%）每轮全量下发。核心集是静态的，激活是关键词/BM25 的，
// 两者都不需要向量。

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// coreToolNames 常驻核心集（按使用频率定，≤15）：文件读写与搜索、命令执行、联网、记忆检索、
// 任务清单、并发子智能体，以及查找其余工具的 tool_search 与调用它们的 call_tool（ADR-0120 决策三：
// 历史内激活，tools 数组整场会话不变）。list_dir / run_command 已被 glob / bash 覆盖，降为延迟加载。
// 回合内的 code_act / finish 由 Agent 循环另行追加。
// 返回新切片（不用包级变量：R1.3 禁全局可变状态）。
func coreToolNames() []string {
	return []string{
		"read_file", "glob", "grep", "write_file", "str_replace_editor",
		"bash", "web_search", "web_fetch", "memory_search",
		"todo_write", "spawn_agents", "tool_search", "call_tool",
	}
}

// CoreToolNames 返回常驻核心工具名（新切片，调用方可修改）。
func CoreToolNames() []string { return coreToolNames() }

// coreSet 本目录生效的核心集：CoreTools 非空时以它为准（测试与定制装配），否则用默认核心集。
func (c *CompositeCatalog) coreSet() []string {
	if len(c.CoreTools) > 0 {
		return c.CoreTools
	}
	return coreToolNames()
}

// IsCore 判断工具是否属于本目录的常驻核心集。
func (c *CompositeCatalog) IsCore(name string) bool {
	for _, n := range c.coreSet() {
		if n == name {
			return true
		}
	}
	return false
}

const toolSearchName = "tool_search"

// lazyActive 懒加载是否生效：工具数超过阈值，且目录里有 tool_search（否则被延后的工具无从激活）。
func lazyActive(entries []protocol.CatalogEntry, threshold int) bool {
	if len(entries) <= threshold {
		return false
	}
	for _, e := range entries {
		if e.Name == toolSearchName {
			return true
		}
	}
	return false
}

func (c *CompositeCatalog) threshold() int {
	if c.LazyLoadThreshold <= 0 {
		return 40
	}
	return c.LazyLoadThreshold
}

// LazyActive 当前目录（按 minTrust 过滤后）是否处于懒加载模式。
func (c *CompositeCatalog) LazyActive(ctx context.Context, minTrust types.TrustTier) bool {
	return lazyActive(c.List(ctx, minTrust), c.threshold())
}

// 索引里每个工具的一句话用途：优先取人工维护的 IndexBrief，缺省回退为描述首句（按词边界截到 briefMaxRunes）。
// MCP / 技能只列名称。
const (
	briefMaxRunes     = 60
	mcpToolsPerServer = 12
	skillIndexMax     = 20
)

// ToolsHint 渲染系统提示里的工具段：未加载工具的索引（按类别分组，一行名称+一句话用途）与使用方法。
// 核心工具已在 function 列表里，不再重复列名。非懒加载模式返回空串（调用方回退到平铺全部工具名）。
//
// 字节稳定（ADR-0105 决策一规则 1）：索引列出**全部**非核心工具，不随本会话的激活情况增减；
// 只随工具集本身（安装期：装卸 MCP/技能）变化。排序全部确定。
func (c *CompositeCatalog) ToolsHint(ctx context.Context) string {
	entries := c.List(ctx, types.TrustCommunity)
	if !lazyActive(entries, c.threshold()) {
		return ""
	}
	var deferred []protocol.CatalogEntry
	for _, e := range entries {
		if !c.IsCore(e.Name) {
			deferred = append(deferred, e)
		}
	}
	var sb strings.Builder
	sb.WriteString("Your function list holds the core tools. Other tools exist but are not loaded; each line is name: purpose.\n")
	sb.WriteString(renderDeferredIndex(deferred))
	sb.WriteString(`To use an unloaded tool: tool_search("<name or keywords>") returns its parameters, then call it with call_tool {"name": "<name>", "arguments": {...}}. Inside code_act, polaris.call("<name>", ...) reaches every tool directly.`)
	return sb.String()
}

// renderDeferredIndex 按类别分组渲染未加载工具。内置工具："名称 用途"；MCP 按服务器分组只列名称；技能只列名称。
func renderDeferredIndex(deferred []protocol.CatalogEntry) string {
	byCat := map[string][]protocol.CatalogEntry{}
	mcp := map[string][]string{}
	var skills []string
	for _, e := range deferred {
		switch e.Source {
		case types.ToolMCP:
			server, short := splitMCPName(e.Name)
			mcp[server] = append(mcp[server], short)
		case types.ToolSkill:
			skills = append(skills, e.Name)
		default:
			cat := toolCategory(e.Name)
			byCat[cat] = append(byCat[cat], e)
		}
	}
	var sb strings.Builder
	for _, cat := range categoryOrder() {
		items := byCat[cat]
		if len(items) == 0 {
			continue
		}
		parts := make([]string, 0, len(items))
		for _, e := range items {
			parts = append(parts, e.Name+" "+toolBriefFor(e.Name, e.Description))
		}
		sb.WriteString("- " + cat + ": " + strings.Join(parts, "; ") + "\n")
	}
	if len(mcp) > 0 {
		servers := make([]string, 0, len(mcp))
		for s := range mcp {
			servers = append(servers, s)
		}
		sort.Strings(servers)
		parts := make([]string, 0, len(servers))
		for _, s := range servers {
			names := mcp[s]
			sort.Strings(names)
			more := ""
			if len(names) > mcpToolsPerServer {
				more = ", …+" + strconv.Itoa(len(names)-mcpToolsPerServer)
				names = names[:mcpToolsPerServer]
			}
			parts = append(parts, s+"("+strings.Join(names, ", ")+more+")")
		}
		sb.WriteString("- MCP servers (tool name = mcp__<server>__<tool>): " + strings.Join(parts, "; ") + "\n")
	}
	if len(skills) > 0 {
		sort.Strings(skills)
		more := ""
		if len(skills) > skillIndexMax {
			more = ", …+" + strconv.Itoa(len(skills)-skillIndexMax)
			skills = skills[:skillIndexMax]
		}
		sb.WriteString("- Skills: " + strings.Join(skills, ", ") + more + "\n")
	}
	return sb.String()
}

// splitMCPName "mcp__server__tool" → (server, tool)；不符合约定的整体归入 "other"。
func splitMCPName(name string) (server, short string) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "other", name
	}
	srv, tool, ok := strings.Cut(rest, "__")
	if !ok {
		return "other", name
	}
	return srv, tool
}

// toolBriefFor 索引用途：人工维护的 IndexBrief 优先（ADR-0120 决策三第 6 条），否则回退描述首句。
func toolBriefFor(name, desc string) string {
	if b := IndexBrief(name); b != "" {
		return b
	}
	return toolBrief(desc)
}

// toolBrief 取描述的第一句，超过 briefMaxRunes 时在词边界截断：此前硬截 22 字符，英文描述被截成
// "Trigger an explicit re…"，模型据此无法判断该用哪个工具。
func toolBrief(desc string) string {
	d := strings.TrimSpace(desc)
	if i := strings.IndexAny(d, "\n"); i >= 0 {
		d = d[:i]
	}
	for _, sep := range []string{"。", "；", ". ", "; "} {
		if i := strings.Index(d, sep); i > 0 {
			d = d[:i]
		}
	}
	d = strings.TrimSpace(d)
	if utf8.RuneCountInString(d) <= briefMaxRunes {
		return d
	}
	r := []rune(d)[:briefMaxRunes]
	if i := strings.LastIndexByte(string(r), ' '); i > briefMaxRunes/2 {
		return string(r)[:i] + "…"
	}
	return string(r) + "…"
}

// IndexBrief 内置工具在索引里的一句话用途（中文，≤24 字，写"做什么"）。新增延迟加载的内置工具应在此补一行；
// 未登记的回退到描述首句。只进系统提示索引，不进 provider schema。
func IndexBrief(name string) string {
	return map[string]string{
		"list_dir":           "列出目录内容（名称/类型/大小）",
		"run_command":        "在受限沙箱跑构建、测试、格式化命令",
		"multi_edit":         "单文件内多处精确替换（原子）",
		"notebook_read":      "读取 Jupyter 笔记本",
		"notebook_edit":      "编辑 Jupyter 笔记本单元格",
		"csv_parse":          "把 CSV 文本解析成 JSON 行",
		"data_query":         "对 SQLite 库执行只读 SQL",
		"diff_text":          "比较两段文本的差异",
		"template_render":    "用 JSON 数据渲染 Go 模板",
		"read_tool_ref":      "取回被卸载的工具输出全文",
		"todo_read":          "读取当前待办清单",
		"core_memory_edit":   "编辑常驻核心记忆块",
		"memory_write":       "写入一条长期记忆事实",
		"memory_append":      "给已有记忆实体追加信息",
		"memory_expire":      "让一条记忆失效",
		"memory_reflect":     "对近期事件做反思归纳",
		"memory_page_in":     "把换出的核心记忆块换回",
		"memory_page_out":    "换出暂不需要的核心记忆块",
		"skill_save":         "把验证过的脚本存为技能",
		"skill_generate":     "从成功轨迹生成快速路径技能",
		"search_extension":   "搜索可安装的插件/技能/MCP",
		"install_extension":  "安装插件、技能或 MCP 服务器",
		"agent_message":      "给运行中的子智能体插话",
		"agent_status":       "查看子智能体运行状态",
		"agent_result":       "取子智能体的输出结果",
		"agent_stop":         "中止子智能体",
		"list_agents":        "列出可委派的智能体角色",
		"transfer_to_agent":  "把独立任务委派给另一智能体",
		"get_task_result":    "轮询异步 MCP 任务结果",
		"automation_save":    "创建或修改定时自动化",
		"automation_list":    "列出自动化任务",
		"automation_delete":  "删除自动化任务",
		"automation_run_now": "立即运行一次自动化任务",
		"job_status":         "查看后台作业状态",
		"job_output":         "读取后台作业最近输出",
		"job_kill":           "终止后台作业",
		"execute_wasm":       "在沙箱运行 WebAssembly 模块",
		"feed_fetch":         "读取 RSS/Atom/JSON Feed",
		"tts":                "本地离线文字转语音",
		"video_analysis":     "按间隔抽取视频关键帧",
		"git_commit":         "暂存并提交 git 改动",
		"git_diff":           "查看 git 差异",
		"get_datetime":       "当前日期时间与时区",
		"sys_probe":          "探测系统资源与环境",
		"list_models":        "列出已启用厂商的可用模型",
		"switch_model":       "下一轮切换模型或思考档",
		"harness_history":    "查看 harness 覆盖层版本历史",
		"harness_rollback":   "回滚 harness 覆盖层",
		"session_search":     "检索以前会话里的对话",
		"session_read":       "读取某个会话的一段对话",
		"goal":               "设置/更新跨回合目标（自动续跑）",
		"checkpoint_list":    "列出文件检查点",
		"checkpoint_restore": "恢复到某个文件检查点",
		"secret_get":         "读取已授权的凭据",
	}[name]
}

// categoryOrder 类别输出顺序（固定，保证字节稳定）。
func categoryOrder() []string {
	return []string{"记忆", "技能与扩展", "智能体", "自动化", "后台任务", "文件与数据", "网络与媒体", "开发", "系统与模型", "其他"}
}

// toolCategory 内置工具归类。未知工具归"其他"，不影响可达性。
func toolCategory(name string) string {
	switch {
	case strings.HasPrefix(name, "memory_"), name == "core_memory_edit", strings.HasPrefix(name, "session_"):
		return "记忆"
	case strings.HasPrefix(name, "skill_"), strings.HasSuffix(name, "_extension"), strings.HasPrefix(name, "knowledge_"):
		return "技能与扩展"
	case strings.HasPrefix(name, "agent_"), name == "list_agents", name == "transfer_to_agent", name == "get_task_result", name == "goal":
		return "智能体"
	case strings.HasPrefix(name, "automation_"):
		return "自动化"
	case strings.HasPrefix(name, "job_"), name == "execute_wasm":
		return "后台任务"
	case name == "multi_edit", strings.HasPrefix(name, "notebook_"), name == "csv_parse", name == "data_query",
		name == "list_dir", name == "run_command",
		name == "diff_text", name == "read_tool_ref", name == "template_render", name == "todo_read":
		return "文件与数据"
	case name == "feed_fetch", name == "video_analysis", name == "tts":
		return "网络与媒体"
	case strings.HasPrefix(name, "git_"), strings.HasPrefix(name, "checkpoint_"):
		return "开发"
	case name == "sys_probe", name == "get_datetime", name == "switch_model", name == "list_models",
		strings.HasPrefix(name, "harness_"), name == "secret_get":
		return "系统与模型"
	}
	return "其他"
}

// SearchAliases 工具的检索别名（中英双语关键词 + 类别名），只喂给 tool_search 的 BM25，不进提示词、不占 token。
// 为什么需要：部分工具描述只有英文，而用户与模型常用中文检索（"语音朗读" 找不到 "Convert text to speech"）；
// 懒加载把这些工具移出了首轮 tools，检索失败就等于工具丢了（ADR-0119 可达性评测发现）。
func SearchAliases(name string) string {
	alias := map[string]string{
		"tts":                "语音 朗读 文字转语音 合成 speech voice audio",
		"video_analysis":     "视频 分析 关键帧 截帧 video frames ffmpeg",
		"search_extension":   "搜索 插件 扩展 技能 市场 安装 plugin extension marketplace mcp",
		"install_extension":  "安装 插件 扩展 plugin extension install",
		"core_memory_edit":   "核心记忆 记忆块 编辑 修改 persona 人设 working memory block",
		"get_datetime":       "时间 日期 现在几点 今天 星期 时区 time date clock now",
		"sys_probe":          "系统 环境 资源 内存 磁盘 CPU 操作系统 os memory disk",
		"diff_text":          "文本 差异 对比 比较 diff compare",
		"csv_parse":          "表格 CSV 解析 逗号分隔 table",
		"data_query":         "数据库 SQL 查询 SQLite database select",
		"git_commit":         "提交 代码 版本 git commit",
		"git_diff":           "差异 改动 代码 版本 git diff",
		"feed_fetch":         "订阅 RSS Atom 新闻 资讯 feed",
		"notebook_edit":      "笔记本 单元格 Jupyter ipynb notebook cell",
		"notebook_read":      "笔记本 单元格 Jupyter ipynb notebook cell",
		"harness_history":    "历史 版本 记录 覆盖层 harness history",
		"harness_rollback":   "回滚 撤销 恢复 版本 harness rollback",
		"template_render":    "模板 渲染 template render",
		"multi_edit":         "批量 修改 替换 多处 edit replace",
		"execute_wasm":       "WebAssembly wasm 运行 执行",
		"list_models":        "模型 列表 厂商 价格 上下文 models providers",
		"switch_model":       "切换 模型 换模型 思考 推理 档位 reasoning model",
		"skill_save":         "保存 技能 脚本 复用 skill",
		"skill_generate":     "生成 技能 固化 skill",
		"memory_write":       "记住 写入 长期记忆 偏好 remember",
		"memory_append":      "追加 补充 记忆 append",
		"memory_expire":      "遗忘 过期 失效 记忆 forget",
		"memory_reflect":     "反思 复盘 总结 经验 reflect",
		"memory_page_in":     "换入 记忆块 page in",
		"memory_page_out":    "换出 记忆块 page out",
		"job_status":         "后台 任务 状态 job",
		"job_output":         "后台 任务 输出 日志 job",
		"job_kill":           "后台 任务 终止 取消 kill job",
		"agent_message":      "子智能体 插话 补充 指令 message",
		"agent_status":       "子智能体 状态 status",
		"agent_result":       "子智能体 结果 输出 result",
		"agent_stop":         "子智能体 中止 停止 stop",
		"list_agents":        "智能体 委派 列表 agents",
		"transfer_to_agent":  "委派 转交 智能体 delegate",
		"get_task_result":    "异步 任务 结果 轮询 async task result",
		"automation_save":    "定时 周期 每天 每周 自动化 任务 计划 cron schedule 管线 pipeline",
		"automation_list":    "定时 自动化 任务 列表 有哪些 cron",
		"automation_delete":  "删除 定时 自动化 任务 cron",
		"automation_run_now": "立即 运行 自动化 任务 触发 run now",
		"read_tool_ref":      "裁剪 卸载 工具输出 引用 offloaded output",
		"checkpoint_list":    "检查点 快照 列表 checkpoint",
		"checkpoint_restore": "检查点 快照 恢复 回退 checkpoint restore",
		"secret_get":         "密钥 凭据 secret credential",
		"list_dir":           "目录 列出 文件夹 ls directory",
		"run_command":        "构建 测试 格式化 命令 build test",
		"session_search":     "以前 上次 聊过 对话 历史 会话 检索 conversation history",
		"session_read":       "读取 会话 对话 历史 上下文 conversation",
		"goal":               "目标 长任务 持续 续跑 自动继续 objective goal",
	}[name]
	if cat := toolCategory(name); cat != "其他" {
		alias += " " + cat
	}
	return strings.TrimSpace(alias)
}
