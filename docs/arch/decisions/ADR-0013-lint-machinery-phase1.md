# ADR-0013: lint 机械化 Phase 1（执行带 1 落地）

- **状态**: Accepted（**已执行完毕** 2026-05-16）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: 全 pkg / `.golangci.yml` / CI

## 上下文

`docs/specs/00-Constitution.md` R1~R8 多条规则**仅由 PR review 守护**，机械化欠缺。结合三层执行带：
- 带 1 lint（本 ADR）/ 带 2 golden（ADR-0012）/ 带 3 对抗审查（ADR-0014）

既有 `.golangci.yml` 仅 12 个基础 linter，**未机械化任何宪法规则**。

源码扫描既有违规规模：
- `Err*` sentinel 全局变量 ≥ 9 处（gochecknoglobals 一开就铺红）
- `var (...)` 块 10+ 处（多为常量 / 错误集合，非真"可变状态"）
- 函数长度 / 嵌套深度 / 圈复杂度未审计

## 决策

**Phase 1 启用四个低附带成本、高 ROI 的 linter；Phase 2 留独立 ADR**。

| Linter | 守护规则 | 配置 | 选定理由 |
|--------|---------|------|---------|
| `depguard` | B1 层依赖方向 + R6 internal 隔离 | 按层 deny 矩阵（见 `.golangci.yml`） | 架构级硬约束，违规 ≈ 真实 bug |
| `errorlint` | R1.2 错误包装 | `errorf=true, asserts=true, comparison=true` | 检测 `fmt.Errorf("...%v", err)` 等不带 `%w`；与 `perrors.Wrap` 互补 |
| `nestif` | R7 嵌套深度 ≤ 3 | `min-complexity: 5` | 起始放宽防初始铺红；后续可逐步收紧 |
| `gocyclo` | R7 圈复杂度 ≤ 15 | `min-complexity: 15` | 与 R7 完全对齐 |

`internal/` 包被任意层引用（不入 deny）；`pkg/edge` ↔ `pkg/governance` 同 L3 互引允许（B1 未明禁）。

### Phase 2（独立 ADR 处置）

| Linter | 推迟理由 |
|--------|---------|
| `funlen` | 既有 100+ 行函数（如 state_machine.go FSM）需先逐文件 review |
| `wrapcheck` | 与 `perrors.Wrap` 交互，需先定义"哪些 external errors 必须 wrap"白名单 |
| `gochecknoglobals` | 9+ 处 `Err*` sentinel 是合规 Go 模式，需先确立"Err* 是 R1.3 例外"ADR（ADR-0001 仅豁免一等公民指标） |

### 既有违规处置策略

依优先级：
1. **真实违例**（depguard 命中跨层 import）→ 修复
2. **R7 阈值超出** → 修复或显式 `//nolint:nestif // 原因` + 关联 ADR
3. **errorlint 命中** → 批量改 `perrors.Wrap` 或 `fmt.Errorf("...%w", err)`

**不采用 baseline 模式**：让违规永久绿色等于规则空转，违背执行带 1 哲学。

## 后果

- **正向**: B1 + R7 + R1.2 三大宪法规则从纸面进入物理执行；CI fail-closed
- **负向**: 首次启用预触发 N 处既有违规需修复或显式豁免；本地需装 golangci-lint
- **反例守护**:
  - 未来如有人提议"加 baseline 锁定既有违规" → 拒绝；baseline = 规则空转
  - 未来如有人提议"加 `//nolint:depguard` 跨层 import" → 必须 link ADR 解释为何 B1 例外

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| Phase 1 一次性全部 linter | 既有违规规模大，单次铺红 → review 疲劳 → lint 信任崩塌 |
| baseline 锁定既有违规 | 规则空转，违背执行带 1 哲学 |
| 完全跳过 lint，依赖对抗审查 | 带 3 覆盖语义反模式；机械可检 R 规则用 lint 更经济 |
| 仅 enable 不 configure | gocyclo 默认 30 远超 R7 的 15；funlen 默认 60 与 R7 冲突；必须显式 align |

## 引用代码

- `.golangci.yml`（实施落地，含完整 depguard 矩阵）
- `.github/workflows/ci.yml`（既有 golangci-lint-action 自动覆盖）
- `docs/specs/00-Constitution.md` R1.2 / R1.7 / R6 / R7（被守护的规则）
- `docs/specs/04-Module-Boundary.md B1`（depguard 规则源）

## 关联 ADR

- [ADR-0001](./ADR-0001-observability-global-singleton.md): R1.3 豁免；Phase 2 gochecknoglobals 启用时需扩充
- [ADR-0012](./ADR-0012-spec-consistency-test.md): 同执行带框架（带 2）
- [ADR-0014](./ADR-0014-adversarial-review-action.md): 同执行带框架（带 3）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-16 | 初稿，Accepted；代码执行待 B 阶段批准 |
| 2026-05-16 | B 阶段完成：`.golangci.yml` 启用 4 linter + 三层 deny 矩阵；test 文件 exclude 4 项；`make build/test` 不受影响（lint 配置不影响 build/test）；首次 CI 触发处理违规按"既有违规处置策略" |
