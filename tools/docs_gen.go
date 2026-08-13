//go:build ignore

// docs_gen 维护 docs/*.md 里的"生成块"——从代码机械提取、易漂移的罗列内容
// （路由表、DDL 表清单、注册表等），用 BEGIN/END 标记包裹，与源保持自动同步。
//
// 背景（local_playground/prompt/docs-optimization-plan.md P4-A）：人工维护的罗列型
// 文档内容（如 M13 §1.2 的 REST 路由表）会随代码演进悄悄漂移——新增/改名的路由不会
// 有人回来同步文档。人工压缩只把这类漂移面缩小一次，生成块 + CI 校验把它永久归零。
//
// 用法：
//
//	go run tools/docs_gen.go              # 重新生成全部注册块，写回文件
//	go run tools/docs_gen.go -check       # 只校验，drift 时退出非零（CI 用，make docs-gen-check）
//
// 标记格式（块内容由生成器函数产出，标记行本身手写、不受影响）：
//
//	<!-- BEGIN GENERATED: <block-id> · 源: <source-desc> · 勿手改，改源后跑 make docs-gen -->
//	...生成内容...
//	<!-- END GENERATED: <block-id> -->
//
// 新增生成块步骤：① 在 generators 里注册 id → 生成函数；② 在目标 .md 文件里手写一对
// BEGIN/END 标记（block-id 与 generators 里的 key 一致）；③ 跑一次 `go run tools/docs_gen.go`
// 落生成内容。生成器函数只返回标记之间的内容，不含标记本身两行。
//
// 刻意的保守设计：生成块只允许**新增**到文档里（附加新小节），不覆盖既有的手写罗列型
// 内容——现有罗列大多混有设计意图/语义注解（如路由表里"[UserInterrupt] 中断（详见
// §1.2.5，<200ms SLO）"这类跨小节引用），并不满足"该块不含设计意图"这条生成化候选
// 判定标准（见 docs-optimization-plan.md P4-A §识别标准）。盲目覆盖会丢信息；本工具
// 只负责把"代码事实的权威快照"打成生成块，供人工核对手写内容是否漂移，最终是否用
// 生成块整体替换某段手写内容，由人工在核对后决定。
package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var beginRe = regexp.MustCompile(`^<!-- BEGIN GENERATED: (\S+) · 源: .* -->\s*$`)

// generators：块 ID → 生成函数。生成函数返回的字符串以换行结尾的多行文本，
// 不含首尾空行、不含标记行。
var generators = map[string]func() (string, error){
	"m13-route-inventory": genM13RouteInventory,
	"arch-index-table": genArchIndexTable,
}

func main() {
	checkOnly := false
	for _, a := range os.Args[1:] {
		if a == "-check" {
			checkOnly = true
		}
	}

	targets, err := findGeneratedBlocks("docs")
	if err != nil {
		fmt.Fprintf(os.Stderr, "扫描生成块失败: %v\n", err)
		os.Exit(2)
	}
	targets = append(targets, findGeneratedBlocksIn("CLAUDE.md")...)

	if len(targets) == 0 {
		fmt.Println("docs-gen: 未发现任何 BEGIN GENERATED 标记（本工具当前无绑定文档，非错误）")
		return
	}

	drift := false
	for _, t := range targets {
		gen, ok := generators[t.id]
		if !ok {
			fmt.Fprintf(os.Stderr, "FAIL: %s:%d 标记了未注册的生成块 id=%q（见 tools/docs_gen.go generators 表）\n", t.file, t.beginLine, t.id)
			os.Exit(2)
		}
		content, err := gen()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: 生成块 %q 执行失败: %v\n", t.id, err)
			os.Exit(2)
		}
		content = strings.TrimRight(content, "\n") + "\n"

		if content == t.current {
			continue
		}
		drift = true
		if checkOnly {
			fmt.Printf("FAIL: %s 中生成块 %q 已漂移（源已变但文档未同步）\n", t.file, t.id)
			continue
		}
		if err := replaceBlock(t.file, t.id, content); err != nil {
			fmt.Fprintf(os.Stderr, "写回 %s 失败: %v\n", t.file, err)
			os.Exit(2)
		}
		fmt.Printf("已重新生成: %s 生成块 %q\n", t.file, t.id)
	}

	if checkOnly {
		if drift {
			fmt.Println()
			fmt.Println("处理方式：本地跑 `make docs-gen` 重新生成并提交。")
			os.Exit(1)
		}
		fmt.Printf("docs-gen-check ok（%d 个生成块与源一致）\n", len(targets))
	}
}

type block struct {
	file      string
	id        string
	beginLine int
	current   string // 现有标记之间的内容（含末尾换行，不含标记行）
}

func findGeneratedBlocks(root string) ([]block, error) {
	var out []block
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			if e.Name() == "decisions" {
				continue // ADR 是历史档案，不参与生成块机制
			}
			sub, err := findGeneratedBlocks(root + "/" + e.Name())
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, findGeneratedBlocksIn(root+"/"+e.Name())...)
	}
	return out, nil
}

func findGeneratedBlocksIn(path string) []block {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []block
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	var cur *block
	var buf strings.Builder
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if cur == nil {
			if m := beginRe.FindStringSubmatch(line); m != nil {
				cur = &block{file: path, id: m[1], beginLine: lineNo}
				buf.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "<!-- END GENERATED: "+cur.id+" -->") {
			cur.current = buf.String()
			out = append(out, *cur)
			cur = nil
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	return out
}

func replaceBlock(path, id, newContent string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inBlock := false
	for sc.Scan() {
		line := sc.Text()
		if m := beginRe.FindStringSubmatch(line); m != nil && m[1] == id {
			lines = append(lines, line)
			lines = append(lines, strings.TrimRight(newContent, "\n"))
			inBlock = true
			continue
		}
		if inBlock && strings.HasPrefix(line, "<!-- END GENERATED: "+id+" -->") {
			lines = append(lines, line)
			inBlock = false
			continue
		}
		if inBlock {
			continue // 丢弃旧内容行
		}
		lines = append(lines, line)
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// ---- 生成器实现 ----

// 逐行匹配（一条路由注册一行），handler 表达式贪婪匹配到行内最后一个右括号之前，
// 正确处理 a2a.AgentCardHandler(s.a2aCfg) 这类 handler 表达式自身带括号调用的情况
// （非贪婪 [^)]+ 会在内层右括号处提前截断）。可选的行尾 // 注释一并剥离。
var routeLineRe = regexp.MustCompile(`(?m)mux\.(?:HandleFunc|Handle)\("(GET|POST|PUT|DELETE|PATCH) ([^"]+)",\s*(.+)\)\s*(?://.*)?$`)

// genM13RouteInventory 从 internal/gateway/server/server_routes.go 提取全部
// method+path+handler，按 path 排序生成表格。只扫这一个文件——它是路由注册的
// 唯一入口（server_init.go 里另有 2 处 mux.Handle("/",...) 是静态资源兜底路由，
// 不属于本节要覆盖的 API 清单，刻意排除）。
func genM13RouteInventory() (string, error) {
	data, err := os.ReadFile("internal/gateway/server/server_routes.go")
	if err != nil {
		return "", err
	}
	type route struct{ method, path, handler string }
	var routes []route
	for _, m := range routeLineRe.FindAllStringSubmatch(string(data), -1) {
		handler := strings.TrimSpace(m[3])
		// handler 表达式里去掉 s. 前缀里的接收者噪音，保留可读的方法路径
		handler = strings.TrimPrefix(handler, "s.")
		routes = append(routes, route{m[1], m[2], handler})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].path != routes[j].path {
			return routes[i].path < routes[j].path
		}
		return routes[i].method < routes[j].method
	})

	var b strings.Builder
	b.WriteString("| Method | Path | Handler |\n")
	b.WriteString("|---|---|---|\n")
	for _, r := range routes {
		fmt.Fprintf(&b, "| %s | `%s` | `%s` |\n", r.method, r.path, r.handler)
	}
	fmt.Fprintf(&b, "\n共 %d 条，提取自 `internal/gateway/server/server_routes.go`（`mux.HandleFunc`/`mux.Handle` 全量扫描，"+
		"不含 `server_init.go` 里的静态资源兜底路由）。本表是代码事实的权威快照，供与上方 §1.2 手写分组罗列交叉核对——"+
		"手写罗列携带跨小节引用与语义分组，不由本表自动替换。\n", len(routes))
	return b.String(), nil
}

// genArchIndexTable 自动计算 docs/arch 下文档的 est_tok（bytes/2.2）并生成表格
func genArchIndexTable() (string, error) {
	// 换算口径：中文为主的 md 约 bytes / 2.2 ≈ token
	const bytesPerToken = 2.2

	type docEntry struct {
		file string
		domain string
		desc string
		path string
		tok int
	}
	
	entries := []docEntry{
		{"`spec/state.yaml`", "SSoT（Single Source of Truth，唯一权威源） 规约", "状态机 + 全模块阈值（唯一权威）", "docs/arch/spec/state.yaml", 0},
		{"`M11-Policy-Safety.md`", "L0 策略", "五防线、Cedar、TaintedString、KillSwitch、PII（Personally Identifiable Information，个人可识别信息） Vault、SSRFGuard", "docs/arch/M11-Policy-Safety.md", 0},
		{"`M07-Tool-Action-Layer.md`", "L1 工具", "见下方 [M07 补充](#m07-补充)", "docs/arch/M07-Tool-Action-Layer.md", 0},
		{"`M02-Storage-Fabric.md`", "L0 存储", "三轴存储、EventLog、MutationBus、Outbox、SchemaManager", "docs/arch/M02-Storage-Fabric.md", 0},
		{"`M05-Memory-System.md`", "L1 记忆", "四层记忆、PromptBuilder、HybridRetriever、Consolidation", "docs/arch/M05-Memory-System.md", 0},
		{"`ARCHITECTURE.md`", "总览", "见下方 [ARCHITECTURE 补充](#architecture-补充)", "docs/arch/ARCHITECTURE.md", 0},
		{"`M04-Agent-Kernel.md`", "L1 内核", "状态机 13 态、S_VALIDATE 四层、System 1/1.5/2 路由、Saga", "docs/arch/M04-Agent-Kernel.md", 0},
		{"`M13-Interface-Scheduler.md`", "L3 接口", "见下方 [M13 补充](#m13-补充)", "docs/arch/M13-Interface-Scheduler.md", 0},
		{"`M13-bis-Extension-Registry.md`", "L3 扩展", "见下方 [M13-bis 补充](#m13-bis-补充)", "docs/arch/M13-bis-Extension-Registry.md", 0},
		{"`M10-Knowledge-RAG.md`", "L2 知识", "文档树、6 阶段摄入、GraphRAG、IncrementalIndexer", "docs/arch/M10-Knowledge-RAG.md", 0},
		{"`M09-Self-Improvement-Engine.md`", "L2 自演化", "五条无梯度路线、SurpriseIndex 完整版、MEMF（Memory of Errors and Mistakes Framework，错误记忆框架）、Auto-Curriculum", "docs/arch/M09-Self-Improvement-Engine.md", 0},
		{"`M06-Skill-Library.md`", "L1 技能", "技能三件套、Logic Collapse（Python+ContainerSandbox）、三级检索", "docs/arch/M06-Skill-Library.md", 0},
		{"`M03-Observability.md`", "L0 可观测", "OTel（OpenTelemetry）、TokenBurnRate（CANONICAL）、SurpriseIndex 基础、AutoConfig", "docs/arch/M03-Observability.md", 0},
		{"`00-Global-Dictionary.md`", "字典", "全 `[Concept]` 标签定义、XR-01~07 跨模块规则、公理", "docs/arch/00-Global-Dictionary.md", 0},
		{"`M01-Inference-Runtime.md`", "L0 推理", "Provider Router、Model Pool、CircuitBreaker、SemanticCache", "docs/arch/M01-Inference-Runtime.md", 0},
		{"`M08-Multi-Agent-Orchestrator.md`", "L2 协同", "Blackboard、CAS（Compare-And-Swap，比较并交换） 认领、Reaper、Supervisor Tree、7 编排模式", "docs/arch/M08-Multi-Agent-Orchestrator.md", 0},
		{"`M12-Eval-Harness.md`", "L3 评测", "EvalCase、五层 Evaluator、TrajectoryReplayer、CI 门控", "docs/arch/M12-Eval-Harness.md", 0},
		{"`ROADMAP.md`", "路线", "时间敏感项 / 工程现状 / 未完成研究方向 / 工程纪律 / 拒绝清单（**人类参考**，AI 默认不加载）", "docs/arch/ROADMAP.md", 0},
		{"`DIAGRAMS.md`", "图谱", "时序图（**人类参考**，AI 默认不加载）", "docs/arch/DIAGRAMS.md", 0},
		{"`Module-Dependency-Axioms.md`", "依赖公理", "包间依赖方向、防循环依赖底线、领域模型正交性", "docs/arch/Module-Dependency-Axioms.md", 0},
	}
	
	for i := range entries {
		st, err := os.Stat(entries[i].path)
		if err == nil {
			size := st.Size()
			// round to K
			tok := int(float64(size) / bytesPerToken / 1000.0)
            if float64(size)/bytesPerToken/1000.0 - float64(tok) > 0.0 {
                tok++
            }
			entries[i].tok = tok
		}
	}
	
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].tok > entries[j].tok
	})
	
	var b strings.Builder
	b.WriteString("| 文件 | 域 | est_tok | 内容摘要 |\n")
	b.WriteString("|------|----|---------|----------|\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "| %s | %s | %dK | %s |\n", e.file, e.domain, e.tok, e.desc)
	}
	return b.String(), nil
}
