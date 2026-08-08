# ADR-0090：注释里的「设计名」与「代码名」——不做门控，改为要求锚定

- 状态：Accepted（已执行）
- 日期：2026-08-09
- 关联：ADR-0089（注释失效路径门控）、`docs/specs/00-Constitution.md` §R2 命名规范字典

## 背景

2026-08-08 建立 `.go` 注释失效**路径**门控后，自然的下一问是：注释里引用的**符号**
是否也该门控？全仓扫描给出 366 个「注释里出现、但代码中一次都没出现过」的标识符，
共 626 处。

逐条复核后，这 366 个分为四类，只有第四类是缺陷：

| 类 | 例 | 数量级 | 判定 |
|---|---|---|---|
| 历史注记 | `UndoFn` / `SagaLog`（「此处曾…该机制结构上是死的」）、`RegisterWorker`（「已删除」） | 最大 | **合法且必要**——不点名旧符号，读者无法把历史 commit 对应到今天 |
| 外部产品 / 论文方法名 | `CosyVoice`、`TencentDB`、`MemAPO`、`ContraPrompt`、`GEPA` | 中 | 合法，本就不是本仓符号 |
| 设计层概念名 | `ModelVersionRegistry`、`AgentHER`、`NumericalConsistency`/`SemanticJudge`（L1~L3 分层名）、`LeaseHeartbeat`、`TripleCtrlCGuard` | 中 | 合法，见下方「锚定」要求 |
| 真漂移 | `handleAgentStreamFSM`、`promoteOrCache` | **7 处** | 缺陷，已修 |

信噪比约 **1:50**。

## 决策一：不为符号引用建门控

做成门控会立刻产生 359 条误报，唯一的收敛方式是往白名单里塞 359 条——而白名单一旦
膨胀到这个量级，门控就名存实亡（同样的循环见 ADR-0089 背景里那 24 条僵尸豁免键）。

**路径可以门控、符号不行**，差别在判定的确定性：路径是否存在由 `os.Stat` 给出二值
答案；符号是否「应该存在」取决于它是历史记录、外部名词还是实现声明——这是语义问题，
静态分析给不出答案。

一次性人工扫描是合适手段（成本约一小时，产出上表），但不该固化为 CI 门控。

## 决策二：设计名必须在其实现处锚定

设计层概念名（M_X 文档、ADR、spec 里定义的子系统名）可以在注释中自由使用，
**但该概念的代码实现处必须有一句把两者绑起来的注释**。已有的正例：

- `internal/llm/modelregistry/registry.go`：`// Registry 是 ModelVersionRegistry 的业务逻辑层，包装 repo.ModelVersionRepository。`
- `internal/learning/reflexion/reflexion.go`：`// ReflexionEngine 是 AgentHER (Hindsight Experience Replay) 的核心引擎。`

没有这句锚定，设计名就成了悬空引用：读者拿着 `ModelVersionRegistry` 全仓 grep 一无所获，
只能猜是没实现、还是改了名、还是记错了。**锚定句把「设计名 ≠ 代码名」从缺陷降级为约定。**

Go 侧不重命名去迁就设计名——`modelregistry.Registry` 比 `modelregistry.ModelVersionRegistry`
更符合 R2.1「Package 单数小写 + 类型名不与包名叠词」，包限定后语义已完整。

### 后果边界

- 本次修掉的 7 处真漂移：`handleAgentStreamFSM`（交互式编排已随 ADR-0085 迁入
  `internal/gateway/session`，4 处仍以「当前实现」口吻指旧名）、`promoteOrCache`
  （已导出为 `PromoteOrCache`，3 处）。
- **重新评估触发条件**：若某次重构后真漂移一次性超过 50 处，说明当时缺的是重构工具
  而非门控——应当在重构脚本里同步改注释，而不是回头再建一个符号门控。
