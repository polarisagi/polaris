# ADR-0117: 权限模式与工具审批矩阵

- **状态**: Accepted
- **日期**: 2026-10-20
- **决策者**: MrLaoLiAI（设计：Claude；实现：Sonnet）
- **相关模块**: M11（`internal/security/policy`、`internal/security/taint`）/ M04（`internal/agent` S_VALIDATE）/ M07（工具能力分级）/ M13（审批卡、设置页、`approval_rules` 表）
- **修订**: ADR-0007 决策四（输入来源分级）；取代 M13 §8.8 / M07 §7.1 中 `interceptComputerUse` 内的硬编码模式开关

## 上下文

此前每个对话都被问烦：用户输入固定 TaintHigh，凡由它派生参数的非只读工具在 S_VALIDATE L1 必然被拦、弹审批；`computer_use_mode` 只管电脑操控，`bash` 另有一套命令风险 HITL，两处各问各的；`feed_fetch`/`web_search` 等只读联网工具被标成 write-network，连"完全访问"下都要审批。同时反面也有漏洞：若干高风险工具被标成只读，加载器对缺失/未知的 `capability` 默认放行。

## 决策

### 决策一：单一权限模式 `permission_mode`

三档 `default`（读取类自动，其余每次询问）/ `auto_review`（默认，读取、工作区内常规操作、已知安全命令自动；其余询问）/ `full_access`（除硬限制外不再询问）。偏好表只认一个键 `permission_mode`，旧键 `computer_use_mode` 在启动与读取时一次性迁移并回写；非法值取最严档 `default`。**每次工具调用实时读取**（`PreferencesRepo.GetPermissionMode`），回合中途切换在下一次工具调用生效。前端设置页 Tab、聊天输入区下拉统一用"权限模式"措辞，不再限定"设备操控"。

### 决策二：能力分级 fail-closed，与放宽同批落地

`CapabilityLevel` 新增 `CapReadNetwork`（读取型联网：`web_fetch`/`web_search`/`feed_fetch`），序为 ReadOnly < ReadNetwork < WriteLocal < WriteNetwork < Privileged。`tool.yaml` 缺失/未知的 `capability`、`risk_level` 加载即报错，不再默认只读；`Tool.EffectiveCapability()` 对 MCP/技能工具 fail-closed（MCP 默认写网络，仅"服务端自报只读且风险 ≤ Medium"才按读取型联网；技能 ≥ 写本地）。同批修正此前错标为只读/低风险的工具（`agent_message`/`agent_stop`/`checkpoint_restore`/`execute_wasm`/`job_kill`/`harness_rollback`/`multi_edit`/`notebook_edit`/`todo_write` → 写本地并补风险等级）。"放宽"（决策三、五）与"收紧"不可只落一半。

### 决策三：owner 本人输入不再视为外部不可信（ADR-0007 决策四）

经**已认证的本地可信客户端**（web/desktop/cli，`Authenticated && ClientType.IsLocalTrusted()`）进入的用户输入标 `TaintLow`；信任在 HTTP 边界由鉴权上下文判定后写入 `session.Request.OwnerTrusted`，不取自请求体自报，且仅当 `Channel` 属本地客户端渠道时生效。外部渠道（telegram/webhook/plugin_channel/cron/workflow）、匿名回环兜底、远程 API、对端节点、Stop hook 续跑文本、工具输出、网页内容、记忆召回一律保持原等级。同一规则贯穿回合中途插话（`SteerOwner`）与 Redirect（`InterruptRequest.OwnerTrusted`）；插话并入全局污点按各自等级取最大值（只升不降）。

### 决策四：确定性授权判定 `policy.Decide`，不调用 LLM

`Decide(mode, tool, args, argTaint, ctx)` 是唯一判定入口，S_VALIDATE 对计划内每个节点调用（三个校验调用点同一函数），取代 `interceptComputerUse` 里的模式开关（只保留 Cedar 预检）。求值顺序固定：**硬限制 → 读取类 → full_access → 已记住授权 → auto_review 自动放行 → 询问**。

- **读取类**（只读、读取型联网、截图等观察类 UI 动作、`str_replace_editor view`）三档都直接执行。
- **自动放行（auto_review）**：工作区可信根（项目目录 + workspace）内的文件写入；`cmdclass` 判为安全的 shell 命令；非文件非 shell 的写本地工具（风险 < High，目标在工作区内）。
- **shell 分类 `cmdclass`**：基于 `mvdan.cc/sh` 语法树逐段分类 `Safe/Unknown/Dangerous/Catastrophic`；复合命令（`; && || |` 子 shell、`bash -c`、`xargs`/`env` 包装）逐段取最严；重定向与路径按工作区解析；`git status; rm -rf ~` 为灾难性，`find -delete`/`-exec rm` 为危险；运行期展开词（`$X`、命令替换）不可判为安全。
- **硬限制（任何档位含 full_access 都询问，且不可被记住的授权覆盖，审批卡无"以后不再询问"）**：
  1. 灾难性命令：删根/家目录、`sudo`/`su`/`doas`、`mkfs*`、`dd` 写块设备；
  2. 凭据外流：本回合读取过凭据（`secret_get`，或同批并行）后的任何可联网操作；
  3. 参数污点 ≥ High 且操作敏感：写网络、特权、危险 shell 命令、工作区外写入、高风险的其它写入、键盘/鼠标类 UI 输入。
- 判定输入只用工具元数据与参数，不读取模型自述；未知模式取 `default`。

### 决策五：审批卡最小化 + 项目级"以后不再询问"

- 审批卡只有"允许/拒绝"两个按钮 + 一个默认不勾选的"以后不再询问：<一行说明>"。**一轮校验里全部待审节点合并成一张卡**（`items[]`）。硬限制项整卡不提供勾选。
- 记住的是**最窄规则**，作用域固定为当前项目：`cmd`（命令前缀，逐词精确匹配，`git` 不放行 `gitx`；多子命令程序取"程序+子命令"，其余取首个选项前至多 3 词；危险段取整段精确）、`domain`（精确域名，不含子域）、`dir`（工作目录外的写入目录）、`app`、`tool`（含 MCP 全名）。含运行期展开的命令不可记住。表 `approval_rules(id, project_id, kind, pattern, created_at, last_used_at, hit_count)`，随项目删除级联清除。
- SSE `approval_required` 事件新增 `reason`、`hard_limit`、`remember_label`、`items[]`；`POST /v1/approvals/{id}/resolve` 新增 `remember`。审批超时一律拒绝（`CheckpointToolApproval`，任何模式）。无审批通道时需询问的调用直接拒绝（fail-closed）。
- 每次自动/规则放行写审计（`permission_auto_allow`，含判定名或规则 ID）；用户批准/拒绝同样留痕；设置页"已记住的授权"逐条可删，项目列表显示"已记住 N 条授权"。
- 放行后为污点 ≥ Medium 的节点铸造按内容哈希匹配的豁免令牌，让 L1 污点闸门与出口污点检查认可这次已授权的调用；工具内部的命令风险 HITL 对"完全访问/已记住授权"的调用让位（`RiskDeny` 硬拒绝不受影响），自动放行的安全命令仍保留工具内二次防线。

### 决策六：拒绝与硬限制的话术

被拒绝或命中硬限制时，工具结果给模型一句原因 + 替代方案（只读核查、在项目目录内完成、告诉用户自行执行），并明确"不要重试同一调用"；同一回合内被拒绝的同一调用不再询问用户。不往系统提示词里加任何关于权限的说明。

## 后果

- **正向**: 日常对话（读取、项目内写入、`go test`/`git status` 类命令）不再弹窗；`feed_fetch` 等只读联网工具在所有档位无需审批；被网页内容诱导的写网络/危险操作在"完全访问"下仍必问。
- **负向**: `code_act` 按特权工具处理，auto 档每次询问（沙箱执行仍是首选，后续可按语言/沙箱等级细分）；`Unknown` 类命令在"完全访问"下允许（硬限制只拦危险以上）；无审批通道（未注入 HITL）的会话遇到需询问的操作仍直接拒绝；有审批通道的无头会话见文末“追记：无人值守审批”。
- **反例守护**: 任何让硬限制可被模式、记住的授权或模型自述绕过的提议，须先给出新的威胁模型并经用户确认；`approval_rules` 不得出现 `kind`/`pattern` 以外的通配语义。

## 追记：无人值守审批（2026-10）

- **决策**：cron / workflow / webhook 等无人值守回合里需询问的调用，**进入 HITL 待审批队列等待用户事后处理**，而不是直接拒绝。审批项与对话内卡片同形（`items[]`、`hard_limit`、`remember_label`，另带 `unattended`、`session_id`）；允许 / 拒绝 / “以后不再询问”，规则按该会话当前项目记忆（自动化本身没有 `project_id`）；硬限制项不提供记住。
- **期限**：默认 24 小时，复用 M13 阈值 `hitl.default_deadline_min_long`（不新增阈值）；超时一律拒绝，模型收到“被拒绝”的工具结果后继续。
- **运行预算**：cron 运行超时在等待审批期间暂停（否则 5–60 分钟的运行超时会在批准前先杀掉运行）；run/automation 状态 `suspended` 显示“等待批准”。
- **可见性**：全局轮询（侧边栏角标）、页面可见 toast、后台且有权限时系统通知；自动化页顶部提示条。桌面壳不新增业务命令，通知走 Web Notification。
- **不变量**：硬限制、污点豁免、fail-closed 语义与前台完全一致；无审批通道仍直接拒绝；`Unattended` 只改变等待期限与可见性，不改变任何判定结果。
- **局限**：workflow/webhook 路径的整体超时尚未随审批暂停（仅 cron 实现了可暂停预算）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 让 LLM 判定"是否安全" | 概率判断不能做授权边界；且提示注入可直接影响判定 |
| 记住的授权跨项目生效 | 一个项目里的信任不应外溢；规则删除也要有明确的归属 |
| 硬限制可在"完全访问"下关闭 | "完全访问"的承诺是少打扰，不是允许被不可信内容远程操作 |
| 每个节点各弹一张卡 | 审批疲劳的直接来源 |

## 引用代码

`internal/security/policy/approval*.go`、`internal/security/policy/cmdclass*.go`、`internal/agent/agent_permission*.go`、`internal/gateway/session/input_taint.go`、`internal/store/repo/repo_approval_rule.go`、`internal/protocol/schema/052_approval_rules.sql`、`pkg/types/enums_tool.go`
