# ADR-0121: 统一权限轴——授权单源、执行环境派生、部署档位只设上限

- **状态**: Accepted
- **日期**: 2026-10-21
- **决策者**: MrLaoLiAI（设计：Claude；实现：Sonnet）
- **相关模块**: M11（`internal/security/policy`）/ M04（`internal/agent` S_VALIDATE）/ M07（`internal/tool/builtin/*`、`guard`、`internal/tool`）/ M06（`internal/action/codeact`、`internal/action/hook`）/ M13（设置页、偏好接口）/ Rust `native_sandbox`
- **修订**: ADR-0115 决策一（信任档位中的授权字段）；ADR-0117 决策四（读取类、判定结论向执行层传递）、决策五末条（工具内二次防线）

## 上下文

现场：`permission_mode=full_access`、`trust_profile=standard`、默认项目（无 `root_path`）下，`read_file` 读仓库被 `path_guard` 拒绝，`bash cat` 却成功。根因是三套互不联动的权限机制：

| 机制 | 时机 | 管什么 | 认 permission_mode |
|---|---|---|---|
| `policy.Decide`（ADR-0117） | 每次调用，运行期 | 是否询问 | 是 |
| `trust_profile` → `Policy`（ADR-0115） | 启动期定死 | bash 是否沙箱、路径是否放开、工具内 HITL 自动放行、run_command 白名单 | 否 |
| `guard` 路径白名单 | 工具内部 | 文件工具可访问根 = DataDir + `allowed_paths` + 项目根 | 否 |

由此产生的缺陷（代码事实）：

1. **批准后仍被拒**：`Decide` 放行（含用户点"允许"）后，工具内部以更窄边界二次拒绝——读写工作区外文件、full_access 下的 bash 仍在沙箱内。
2. **可信根两套口径**：`Decide` 的根 = 项目根 + workspace；`guard`/沙箱的根 = **DataDir（`~/.polaris` 整个根）** + `allowed_paths` + 项目根。
3. **自保护缺口**：读类工具只查白名单不查黑名单，而白名单含 `~/.polaris`——模型可 `read_file ~/.polaris/vault.key`（主密钥）、`secrets/`；写黑名单只列子目录，`config.toml`、`vault.key`、`KILLSWITCH`、`.fullstop` 可被改写/删除（可自行把 `trust_profile` 改成 full，违反 HE-7）；读类工具不做软链真实路径复核。
4. **平台不一致**：macOS Seatbelt 全局 `(allow file-read*)`，沙箱内 bash 可读全盘（含 `~/.ssh`）；Linux bwrap 只挂白名单。
5. **旁路**：`code_act` 工具桥的内层调用不经过 `Decide`（full 档全部放行、其余档只读）；hooks 收到的 `permission_mode` 恒为 `"default"`，PreToolUse 的 `ask` 一律按拒绝处理。

## 决策

**结论：`policy.Decide` 是唯一的授权判定；执行层（路径边界、沙箱与否、工具内 HITL）只由"本次调用的授权结论"派生，不得以更窄的边界否决已授权的调用；`trust_profile` 退为部署期预设，与授权相关的只剩"权限上限"。**

### 决策一：三层职责

| 层 | 唯一来源 | 内容 |
|---|---|---|
| 授权 | `policy.Decide(mode, …)`，mode = `min(偏好 permission_mode, Policy.MaxPermissionMode)` | 允许 / 询问 / 拒绝 |
| 执行环境 | 本次调用的**授权凭据**（grant，见决策二） | 路径边界、宿主或沙箱、工具内 HITL 是否让位 |
| 部署预设 | `trust_profile` | 权限上限（hardened ≤ `auto_review` 并强制容器沙箱）+ 与授权无关的功能开关（快照、`secret_get`、sleep-time、子代理并发/深度、出站污点、PII、SSRF 审计、code_act 污点阻断） |

`Policy` 删除 `SandboxDisabled`、`AllowedPathsOpen`、`HITLAutoApprove`、`RunCmdWhitelistEnabled`、`CodeActAllToolsAllowed`，新增 `MaxPermissionMode`（standard/full = `full_access`，hardened = `auto_review`）。`trust_profile=full` 不再单独带来宿主执行。偏好接口在 hardened 下拒绝写入 `full_access`。

### 决策二：授权凭据（grant）随调用下发

`authorizePlan` 对每个放行节点记录按 `toolCallKey(name, args)` 匹配的凭据，执行时写入 ctx：

- `GrantExplicit`：`Auto == "full_access"`、命中已记住的授权、用户在审批卡上允许。
- `GrantAuto`：其余自动放行（读取类、`workspace_write`、`safe_command`、`local_write`）。
- 无凭据：未经 `Decide` 的执行路径（fail-closed 兜底）。

`protocol.IsPreApproved(ctx)` 保留语义 = `GrantExplicit`。凭据只对参数逐字节相同的那次调用有效；`code_act` 工具桥的内层调用必须重新判定并**覆盖**外层 ctx 的凭据，不得继承。

### 决策三：执行层按凭据派生

| | GrantExplicit | GrantAuto | 无凭据 |
|---|---|---|---|
| 文件工具路径 | 只拦自保护路径 | 只拦自保护路径（目标已由 Decide 判定） | 可信根白名单 + 敏感/自保护黑名单 |
| bash / run_command | **宿主执行**、不限路径、允许后台作业与持久终端、run_command 白名单不生效；hardened 仍强制容器 | 沙箱（遵从 `sandbox.enabled`），沙箱路径 = 可信根 | 同左，且 run_command 白名单生效 |
| 工具内命令风险 HITL / 异常 HITL | 让位（`RiskDeny` 不变） | 保留 | 保留 |

宿主执行意味着该命令拥有 OS 用户的全部权限——这就是"完全访问"的承诺；防线是 `Decide` 的硬限制（灾难性命令、凭据外流、高污点敏感操作，以及决策四的敏感/自保护路径引用）。

### 决策四：路径分三类，单一事实源 `policy` 包

- **可信根**：项目工作目录 + `Layout.Workspace` + `sandbox.allowed_paths`。`Decide` 的 `Context.Roots` 与工具/沙箱白名单用同一组；**DataDir 根不再进入白名单**（工具确需的 DataDir 子目录逐个列入，且不得含自保护路径）。
- **自保护路径（任何模式拒绝，不可询问、不可记住）**：Polaris 自身的 `vault.key`、`config.toml`、`KILLSWITCH`、`.fullstop`、CLI 会话文件、`secrets/`、`data/`、`audit/`、`eval/`、`config/`、`harness_overlay/prompt_addenda`。`Decide` 新增 `VerdictDeny`（原因 `self_protected`）；文件工具对原路径与软链真实路径都校验。
- **敏感路径（任何模式硬限制询问，不可记住）**：`~/.ssh`、`~/.gnupg`、`~/.aws`、`~/.netrc`、`~/Library/Keychains`，以及写入 `/etc`、`/usr`、`/bin`、`/sbin`。

读取类在三档都自动放行（维持 ADR-0117），但读写目标落入上述两类时分别拒绝或硬限制。`classifyOp` 对读取类工具同样解析路径参数。shell 中能静态解析到这两类路径的词按同等级处理；运行期展开词无法静态判定——宿主执行下 shell 的自保护是尽力而为（显式接受，见后果）。

### 决策五：沙箱物理边界补齐

Seatbelt 配置在全局 `file-read*` 之后追加对自保护与敏感路径的 `deny file-read* / file-write*`（`SandboxContext.DenyPaths`）；bwrap 不挂载这些路径（可信根改为 Workspace 后 `~/.polaris` 不再被 bind）。

### 决策六：旁路收口

- **code_act 工具桥**：内层每次调用经调用方注入的授权接口走 `Decide`（同一 mode 与 Context）；`Allow` 带对应凭据执行，`Ask` 直接拒绝并回"需在外层单独调用以便审批"（桥内无审批通道）。删除 `CodeActAllToolsAllowed`。
- **hooks**：`Input.PermissionMode` 取本次实际模式，按 Claude Code 兼容名映射：`default→default`、`auto_review→acceptEdits`、`full_access→bypassPermissions`。PreToolUse 返回 `ask` 时，有审批通道则走审批（与 bash 内 HITL 同一通道），无通道才拒绝。
- **第三方扩展代码**（MCP 服务进程、技能脚本、Wasm 模块）的执行环境**不随** permission_mode 变化：完全访问是对模型行动的信任，不是对第三方代码的信任。它们的工具调用照常经 `Decide`。

## 后果

- **正向**: 用户看到的"权限模式"就是全部授权语义；批准即生效，不再有"允许了却被拒"；读写自保护在所有档位、所有平台成立；两套可信根合一。
- **负向**: `full_access` 下 bash 在宿主执行，shell 内对自保护路径的运行期展开引用（`cat $(echo …)`）无法静态拦截——与此前 `trust_profile=full` 的风险面相同，只是入口变为设置页；需要物理保证的部署用 `hardened`。`trust_profile=full` 用户若期望"无需切换模式即宿主执行"，须改用 `permission_mode=full_access`。
- **反例守护**: 任何在工具/沙箱层"再加一道比 Decide 更窄的授权"的提议，须改为在 `Decide` 内表达；任何让自保护路径可被模式、记住的授权或审批放开的提议，引用本 ADR 驳回；`code_act` 桥或其他执行入口不得继承外层凭据。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 只在 full_access 下放开 read_file 路径 | 修症状；写入、bash 沙箱、code_act、hooks 仍各自为政 |
| full_access 下 bash 仍走宽松沙箱（默认允许 + 黑名单拒绝） | macOS 嵌套 `sandbox-exec` 失败（swift build、部分测试工具），违背"能跑各种命令"；Windows 无对等机制 |
| 合并 trust_profile 进 permission_mode（删除档位） | 快照、`secret_get` 注册、sleep-time 等是启动期装配且影响工具数组字节稳定（ADR-0120），不宜运行期切换 |
| 读取工作区外一律询问 | 与 ADR-0117"读取类三档自动"冲突，且需新增规则种类与 DDL；敏感/自保护分类已覆盖真实风险 |

## 引用代码

`internal/security/policy/{approval,approval_ops,profile,protected_paths}.go`、`internal/agent/agent_permission*.go`、`internal/agent/agent_capability.go`、`internal/protocol/context.go`、`internal/tool/builtin/guard/guard.go`、`internal/tool/builtin/{bash,run_command,read_file,…}`、`internal/tool/tool_policy.go`、`internal/action/codeact/bridge.go`、`internal/action/hook/firer.go`、`internal/gateway/server/sysadmin/preferences.go`、`cmd/polaris/boot_tools.go`、`rust/substrate/src/native_sandbox/seatbelt.rs`
