# 05 AI 编码工作流（Spec-First 过程）

> 每次 AI 编码前必须加载本文件并按流程执行，不可跳过。

## W0 故障排查工作流

排障（起点只有一个症状）与编码（已知要改什么）是两种信息需求，不混用流程。

1. 先查 `docs/arch/INDEX.md §0 症状索引`。命中 → 直接跳对应文件精读，不从关键词开始全仓库 grep。
2. 未命中 → `grep -ri <症状关键词> docs/arch/decisions/`（是否已有同类 ADR）→ 定位可疑 `M_X` 精读。
3. **结论必须有实测依据**：日志时间线、直接调用受影响接口/函数复现、读实际代码路径，三者至少其一。禁止仅凭"改动时间点接近"或"理论上可能"下因果结论（真实反例见 §0 索引第 5 行）。
4. 定位根因后**强制**在 `docs/arch/INDEX.md §0` 补一行（症状特征 / 归类模块 / 根因类别 / 排查起点），不补则经验无法复用。

## W1 会话初始化

加载顺序与文件清单见 `docs/specs/INDEX.md` 加载策略表；按需加载，不预读 M_X。PR 描述用 W6.3 模板。

## W2 Spec-First 四阶段

**阶段 0 — 上下文锚定**（AI 自行执行，不要求声明）

写代码前按序读取：

1. 目标模块的 `internal/<X>/CLAUDE.md`（如存在）
2. `07-Reference-Implementation.md` 中对应模块的 canonical 文件
3. 相关 ADR：`grep -ri <关键词> docs/arch/decisions/` 至少试一次
4. `docs/specs/CHANGELOG.md` 最近 5 条

**阶段 A — 定义契约**：跨模块 / 契约 / Schema 变更先产出设计（ADR 或计划文件）再实现；单模块内变更直接实现（ADR-0112）。设计含：

```
## 接口契约
- 修改文件 / 新增文件（确需才加）
- 影响模块: 包名列表
- 接口变更: 签名前后对照
- 是否需要 internal/protocol/ 变更: 是/否 + 具体变更
- Schema 变更（涉及时，见 W7.4）
```

**阶段 B — 测试与实现同提交**：覆盖正常 / 边界 / 失败路径；不强制逐文件先测后写的顺序。

**阶段 C — 自查与交付证据**

- [ ] `00-Constitution.md` R1 反模式逐条无违例（含 R1.14 安全门 nil fail-closed、R1.15 MCP 子进程 `sanitizeParentEnv()`）
- [ ] R3 HE-1~7 每条有交代
- [ ] 新增标识符符合 R2 命名字典
- [ ] `make test`、`make build` 通过
- [ ] 无顺手重构未损坏代码（100% 指令溯源）
- [ ] **交付证据**：声明「完成 / 通过 / 全绿」须附实际执行的命令与关键输出（`make lint`、`go test`、对应门控）；未运行的检查写明「未运行」及原因。验收他人（含其他模型、子代理）交付时先自行复跑，不采信自述（ADR-0112）

## W3 变更影响声明

阶段 A 的设计即变更影响声明；PR 提交时浓缩为 W6.3 模板，不重复维护。

## W4 引用纪律

引用必须具体：架构文档 = 文件名 + 章节号；规范 = 文件编号 + 条款号（如 `00-Constitution.md R1.1`）；代码 = 文件名 + 行号。禁止笼统"见架构文档"。

## W5 对话纪律

- 不保留无关历史上下文；不加载无关 M_X；`spec/state.yaml` 按章节 offset/limit 局部读
- 长任务中断后从计划文件 `§进度` 续接（见 W8），不依赖会话历史（ADR-0112）

## W6 PR 纪律

### W6.1 原子变更

1 PR = 1 逻辑变更，体积约束见 `00 R8`（非测试 diff > 1000 行须引用 ADR 或计划文件）；不夹带"顺手修改"。

### W6.2 契约与实现分离

已废止 `[spec]`/`[impl]` 分提交，见 ADR-0112；破坏性契约变更仍走 `04 B5.2`。

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
- [ ] 06 C1~C11 全部勾选
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

## W8 多代理协作（ADR-0112）

1. **分工**：设计方（架构 / 方案）与实现方（编码 / 文档修改）可为不同模型；设计方的计划文件即契约，实现方不得改动契约范围外的文件。
2. **计划文件**：多步骤或跨会话任务在 `local_playground/<任务>/PLAN.md` 落盘，含目标、硬约束、文件分配、`§进度` 节；中断后从 `§进度` 续接。
3. **文件所有权**：并行执行时每个文件只分配给一个执行者；`make docs-sync`、`make docs-gen`、`make gen-threshold-examples`、`make generate-manifest` 等改写共享文件的工具只由收尾者在并行结束后统一运行。
4. **汇报是待核实的陈述**：子代理汇报中的事实结论须由设计方抽查或复跑门控后才算成立。
5. **冲突只记录不改**：执行中发现规范与代码冲突，记入汇报交设计方裁决，不得顺手改规范或改代码迁就。
