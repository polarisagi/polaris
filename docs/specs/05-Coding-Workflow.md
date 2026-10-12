# 05 AI 编码工作流（Spec-First 过程）

> 每次 AI 编码前必须加载本文件并按流程执行，不可跳过。

## W0 故障排查工作流

排障（起点只有一个症状）与编码（已知要改什么）是两种信息需求，不混用流程。

1. 先查 `docs/arch/INDEX.md §0 症状索引`。命中 → 直接跳对应文件精读，不从关键词开始全仓库 grep。
2. 未命中 → `grep -ri <症状关键词> docs/arch/decisions/`（是否已有同类 ADR）→ 定位可疑 `M_X` 精读。
3. **结论必须有实测依据**：日志时间线、直接调用受影响接口/函数复现、读实际代码路径，三者至少其一。禁止仅凭"改动时间点接近"或"理论上可能"下因果结论（真实反例见 §0 索引第 5 行）。
4. 定位根因后**强制**在 `docs/arch/INDEX.md §0` 补一行（症状特征 / 归类模块 / 根因类别 / 排查起点），不补则经验无法复用。

## W1 会话初始化

加载顺序与文件清单见 `docs/specs/INDEX.md` 加载策略表；启动必读总预算 ≈ 12K token，不可再增。进入编码前必须输出 W2 阶段 0 锚定声明 + W6.3 PR 描述模板。

## W2 Spec-First 四阶段

**阶段 0 — 上下文锚定**（AI 自行执行，无需 Human 审核）

写代码前按序读取，并在响应开头声明已读：

1. 目标模块的 `internal/<X>/CLAUDE.md`（如存在）
2. `07-Reference-Implementation.md` 中对应模块的 canonical 文件
3. 相关 ADR：`grep -ri <关键词> docs/arch/decisions/` 至少试一次
4. `docs/specs/CHANGELOG.md` 最近 5 条

声明格式：

```
已锚定上下文:
- 模块约束: internal/extension/CLAUDE.md
- 标杆:    internal/memory/memory.go
- 相关 ADR: ADR-0007（污点降级路径）
- 规范增量: 2026-10-02 R7 文件行数软 800 / 硬 850
```

未声明 → Human 拒绝进入阶段 A。

**阶段 A — 定义契约**（AI 输出，Human 审核；批准前不写任何实现代码）

```
## 接口契约
- 修改文件 / 新增文件（确需才加）
- 影响模块: 包名列表
- 接口变更: 签名前后对照
- 是否需要 internal/protocol/ 变更: 是/否 + 具体变更
- Schema 变更（涉及时，见 W7.4）
```

**阶段 B — 先测试，后实现**（按文件粒度）

每个文件：写 `_test.go`（边界、正常、失败路径）→ `go build` 通过 → 写实现（填空，不改签名）→ `go test` 通过。单文件内可交替写，跨文件必须按顺序。

**阶段 C — 自查**

- [ ] `00-Constitution.md` R1 反模式逐条无违例（含 R1.14 安全门 nil fail-closed、R1.15 MCP 子进程 `sanitizeParentEnv()`）
- [ ] R3 HE-1~6 每条有交代
- [ ] 新增标识符符合 R2 命名字典
- [ ] `make test`、`make build` 通过
- [ ] 无顺手重构未损坏代码（100% 指令溯源）

## W3 变更影响声明

阶段 A 输出的契约即变更影响声明；PR 提交时浓缩为 W6.3 模板，不重复维护。

## W4 引用纪律

引用必须具体：架构文档 = 文件名 + 章节号；规范 = 文件编号 + 条款号（如 `00-Constitution.md R1.1`）；代码 = 文件名 + 行号。禁止笼统"见架构文档"。

## W5 对话纪律

- 不保留无关历史上下文；不加载无关 M_X；`spec/state.yaml` 按章节 offset/limit 局部读
- **一任务一会话**：每个独立逻辑变更（单函数 Spec、单 bug 修复）开新会话，上轮成果作为基线传入。超 20 轮的长会话注意力衰减，规范遗忘风险显著上升

## W6 PR 纪律

### W6.1 原子变更

1 PR = 1 逻辑变更，diff 上限见 `00 R8`；超出须拆分，不可拆则写 ADR 并在标题加 `[oversized]`；不夹带"顺手修改"。

### W6.2 契约与实现分离

阶段 A 的契约变更（`internal/protocol/` 改动）独立 commit，message 加 `[spec]`，只含接口/类型/常量定义；实现 commit 紧随，加 `[impl]`。破坏性变更走 `04 B5.2`。

### W6.3 PR 描述模板

```markdown
## 变更类型
feat / fix / refactor / docs / test；含破坏性变更则 link ADR

## 锚定上下文
- 参考实现: internal/xxx/yyy.go | 对齐 或 偏离（写明原因）
- 相关 ADR: ADR-NNNN（无可省）

## 变更摘要
- 总文件数: N（新增 M，修改 K，删除 0）；diff 行数: __
- 核心逻辑变更: 2 句话
- 高风险区域: 文件名 + 风险
- 建议 reviewer 重点关注: 文件名 + 理由
- 测试覆盖率: 新代码 __%

## 自查
- [ ] 06 C1~C9 全部勾选
```

### W6.4 评审者并行触发

PR 创建后 CI 触发独立 AI reviewer（对抗审查），与人类 review 并行。AI reviewer 仅喂 Diff + `00-Constitution.md`，要求"指出违例并引用 R 编号，无违例输出 NONE"。

## W7 Schema 变更流程

对应根 `CLAUDE.md §编码约定` 的 DDL 修改策略。触发：任何涉及 `internal/protocol/schema/` 的变更。

**W7.1 Phase 判断（第一步）**：读 `CLAUDE.md §当前阶段` 一节（仅该节），标注"上线后" → W7.3；否则 → W7.2（当前默认）。

**W7.2 上线前：直接改建表文件**

1. 定位 `internal/protocol/schema/NNN_<name>.sql`，直接改 `CREATE TABLE`（增列、改类型、加索引）
2. 禁止新建 `NNN_*.sql` 补丁文件（哪怕编号更高；门控：`Test_inv_NoAlterTableInSchema`）
3. 删开发库 `rm ~/.polaris/data/polaris.db`，`make build && bin/polaris` 自动重建
4. 提交前 `git diff --name-only` 不应出现新增的 `schema/*.sql`

**W7.3 上线后：迁移文件**（仅 Phase=生产）

1. `ls internal/protocol/schema/ | tail -1` 确认最大编号，新建 `NNN_<描述>.sql`（max+1）
2. 只写 `ALTER TABLE` / `CREATE INDEX` / `INSERT`（禁 `CREATE TABLE`）；历史文件只读
3. PR 标题加 `[migration]`

**W7.4 阶段 A 补充**（涉及 Schema 时）：契约须含目标文件、变更类型（新增列 / 修改索引 / 合并文件；上线前不可能是新增补丁文件）、受影响的 Go 文件（引用该表的 query 字符串）。
