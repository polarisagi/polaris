# ADR-0096: 桌面版/命令行版形态——守护进程 + 薄客户端，外壳用 Tauri v2 sidecar

- **状态**: Accepted（2026-09-21 确认；P0 守护进程契约化与 P2 外壳骨架已落地）
- **日期**: 2026-09-21 | **模块**: `cmd/polaris/`, `internal/gateway/server/`, `internal/sysmgr/updater/`, `internal/cli/`（删除）, `desktop/`（新增）, `.github/workflows/release.yml`

## 背景

网页版之外要做桌面版与命令行版。三条既有事实决定了形态，任何方案不得与之冲突：

1. **命令行版已经存在**。`cmd/polaris/cli.go` 文件头写明「全部通过 HTTP 与本地运行的 Polaris 服务通信，不直接访问数据库」，chat/status/config/vault/skill/eval 等子命令已接线。所谓"做 CLI"实为补齐命令面与服务发现，不是新建一套。
2. **零 CGO 交叉编译是发布流水线的硬前提**。`release.yml` 以 `CGO_ENABLED=0` 在单 runner 上交叉编译 5 平台，Rust 产物 `libsubstrate` 预编译分发。把 GUI 框架编入主二进制即摧毁该流水线。
3. **守护进程承载窗口之外的能力**。定时调度（`internal/automation`）、TG/Discord 通道（`internal/channel`）、HITL 审批（`internal/automation/hitl`）、OpenAI 兼容 API 均要求进程在无窗口时继续运行。

## 决策一：三端共用唯一业务通道，外壳不得承载业务 IPC

**客户端（Web / 桌面 / CLI / 第三方）与内核之间只有 `HTTP /v1/* + SSE` 一条路。**

- 桌面外壳的 Rust 侧只允许做：子进程管理、窗口、托盘、系统通知、深链、外壳自更新。**禁止任何业务语义的 Tauri command**——其 command 清单即白名单，新增须先改本 ADR。
- 桌面端能力必须是 CLI 能力的子集（同一组 API），防止桌面端私自长出旁路能力。
- **反例守护**：任何"为性能/便利在外壳里直连内核函数"的提议按本条驳回。绕过 gateway 中间件即同时绕过鉴权、限流、审计与 OTel 埋点（HE-1 / HE-3 / HE-7）。
- **同一条规则适用于服务注册**（2026-09-21 实施时发现）：`scripts/install.sh` / `install.ps1` 此前各自手写 launchd plist 与计划任务，与 `polaris service install` 构成两份实现，且标签不同（`com.polarisagi.polaris` vs 新写的标签）——两份都装上就是两个注册项，第二个实例启动时被单实例锁拒绝，而用户看到的只是"服务时好时坏"。现已归并：脚本调用 `polaris service install`，单元文件只有 Go 一处生成，标签沿用既有的 `com.polarisagi.polaris`（Windows 任务名沿用 `PolarisAGI-Polaris`）。

## 决策二：外壳选 Tauri v2 + sidecar 进程，否决 Wails

**Go 二进制作为 sidecar 独立进程随外壳分发，外壳不链接内核。**

| 驳回 Wails 的依据 | 后果 |
|---|---|
| macOS/Linux 构建需 CGO | 与 `CGO_ENABLED=0` 交叉编译矩阵冲突，5 平台发布流水线需重做 |
| Go 方法自动绑定为前端 JS 调用 | 在 HTTP 之外开第二条业务通道，绕过 gateway 中间件（违反决策一） |
| 改用 AssetServer 挂 gateway handler 以避开绑定 | SSE 等流式响应支持存疑，而 `web/src/js/sse.js` 是主链路 |
| 框架进程即宿主进程 | 守护进程生命周期被窗口绑架，与背景事实 3 冲突 |

- **代价边界**：Tauri **不支持交叉编译**，CI 必须引入 macos / windows / ubuntu 三种 runner，与现有单 runner 交叉编译矩阵是并行的两套逻辑，不得合并。
- Rust 工具链已是仓内一等公民（`rust/substrate`），本决策不新增语言栈。

## 决策三：webview 加载 `http://127.0.0.1:<port>`，不把 `web/dist` 作为外壳静态资源

**前端零改动的唯一路径。** 同源之下相对路径 `fetch('/v1/...')`、`EventSource`、Cookie 鉴权全部原样工作；若把 dist 装入外壳，则跨源、鉴权、SSE 三处都要另开机制。

- **代价边界**：窗口必须等待守护进程就绪 → 启动闪屏窗口 + 就绪轮询（指数退避）+ 超时错误窗口（附"打开日志"入口）。
- **附带红利**：同一外壳可连远程 polaris 实例（地址簿），无需额外架构。
- **唯一例外**：外壳内置一张**纯静态故障页**（核心未安装 / 启动超时 / 版本不兼容三种状态），用于 webview 无法加载守护进程时显示。该页不得调用任何 `/v1` 端点、不得含业务 UI——这是决策一的唯一例外，边界由本条锁死，不得以"顺手加个设置页"为由扩张。

## 决策四：sidecar 与 dylib 安装在 app bundle 之外，外壳只是启动器

**约束（2026-09-21 补充）**：本项目**没有 Apple Developer ID、没有 Windows 代码签名证书**，且短期不打算购买。初稿的"桌面版由 Tauri updater 整体更新已签名 bundle"以持有证书为前提，前提不成立，故改为下述布局。

**安装布局**（三平台一致）：

| 组件 | 安装位置 | 更新者 |
|---|---|---|
| `polaris` 二进制 + `lib/`（含 `libsubstrate`） | `~/.polarisagi/polaris/bin/`（`config.DataLayout.Bin`，用户级，免管理员权限） | `sysmgr/updater`，**行为与 tarball 渠道完全一致**（ADR-0095 信任模型不变） |
| 桌面外壳（`.app` / `.exe` / AppImage） | 平台常规位置 | 重跑安装脚本，或外壳自身的 Tauri updater |

- 外壳启动时以固定路径拉起 `~/.polarisagi/polaris/bin/polaris`，**不使用 Tauri 的 bundle 内 sidecar 机制**。
- 由此 `updater_install.go` 的就地替换（`os.Rename` 覆盖二进制与 `lib/`）**永远发生在 bundle 之外**，不触碰任何已签名结构。托管标记 `POLARIS_MANAGED_BY` 与 `updater` 的"拒绝安装"分支**不需要实现**。
- 外壳与守护进程版本解耦，故 `/healthz` 响应带版本号（2026-09-21 已实现，`server_handlers.go`），外壳在版本不兼容时提示而非崩溃。
- **反例守护**：驳回"把 Go 二进制作为 Tauri sidecar 打进 bundle"一类提议——那会让守护进程的自更新与 bundle 完整性耦合；一旦将来取得签名证书，耦合的代价是自更新整条链路作废（见重评触发条件 2）。
- 本决策与 ADR-0095 的分工：0095 定"校验值凭什么可信"，本条定"被替换的文件放在哪里"。

## 决策五：本地令牌默认强制，回环豁免取消

**现状事实**：`internal/gateway/server/middleware_auth.go` 在未配置 `POLARIS_API_KEY` 时把"来自回环"本身当作唯一凭证，并授予 `ClientTypeWebUI` 完整权限。服务器场景下这是合理妥协；桌面场景下本机任意进程、以及浏览器页面经简单请求均可取得完整权限，不可接受。

三种凭证携带方式，覆盖全部客户端且**前端一行不改**：

| 客户端 | 携带方式 |
|---|---|
| CLI / 桌面外壳 / 脚本 | `Authorization: Bearer` 或 `X-API-Key`，令牌读自 `run/polaris.token`（0600） |
| Web UI | gateway 返回 `index.html` 时下发 `Set-Cookie: polaris_local=<token>; HttpOnly; SameSite=Strict; Path=/` |
| 远程部署 | `POLARIS_API_KEY`（现状不变） |

- 选 Cookie 而非改写前端：同源 `fetch` 与 `EventSource` 自动携带，20 余处调用点与 `sse.js` 无需改动；`HttpOnly` 阻断页面内 JS 读取；`SameSite=Strict` + Origin/Host 校验阻断 CSRF 与 DNS rebinding。
- **破坏性变更**：逃生阀 `POLARIS_ALLOW_ANONYMOUS_LOOPBACK=1`（默认关）给"本机裸调 `/v1/chat/completions`"的第三方 OpenAI 兼容客户端留迁移期。健康端点白名单（`/healthz` `/readyz` `/metrics` `/.well-known/agent-card.json`）豁免不变。
- `ClientType.IsLocalTrusted()` 的三个取值与新令牌分支须一次对齐，**不得留下第二条隐式信任路径**（GR-9-001 修过的同类问题）。
- **反例守护**：无令牌的回环 POST 必须 401，以实跑用例锁死；任何"本机即可信"的回归提议按本条驳回。

> 2026-09-25 追记（鉴权冷却与本地令牌的判定顺序）：本地令牌校验（请求头 / Cookie）**先于** IP 冷却判定；远程 `POLARIS_API_KEY` 仍在冷却之后。
> 实证：守护进程每次启动轮换令牌，重启后仍在运行的旧外壳/旧页签持旧令牌轮询，连续 401 触发按 IP 的 5 分钟冷却；回环上所有本机客户端共用 127.0.0.1，持**正确**令牌的 CLI、新窗口一并被 429 锁死（2026-09-25 实测）。
> 安全边界不变：本地令牌由守护进程随机生成 256 位，暴力枚举不可行，冷却对它无防护价值，只剩误伤；冷却本意是限制对**用户自设、可能弱口令**的 API Key 的猜测，故 API Key 分支保持原位。持错误令牌的请求照常计失败、照常被锁。

## 决策六：`internal/cli` 删除；服务发现下沉为 `internal/runtimeinfo`

`internal/cli` 是 `tools/baselines/wiring-allowlist.txt` 在册的未接线债务（`AgentREPL` / `RateLimiterMiddleware` / `WebSocketHub`），且 `AgentREPL` 依赖 `InferFn` 直连内核，与决策一正面冲突。

- **删包**，同步移除 wiring-allowlist 条目并按该文件既有格式在「已删除」段落留记录。
- **不改造为客户端 SDK**：把包名语义从"引导契约"偷换成"客户端 SDK"会使 ADR-0088 台账失去可追溯性。CLI 的 HTTP 客户端代码留在 `package main`，出现第二个 Go 消费方前不抽象（CLAUDE.md「禁止超前抽象」）。
- **唯一下沉项** `internal/runtimeinfo`：`polaris.pid`/`polaris.port`/`polaris.token` 的原子写入、读取与权限校验。**路径本身不在该包定义**——`internal/config.DataLayout` 是路径 SSoT（其文件头明确「所有子系统必须从此结构取路径，禁止各自拼接」），故 `run/` 目录与三个文件路径作为 `Run`/`RunPID`/`RunPort`/`RunToken` 字段加入 DataLayout，runtimeinfo 只接收路径、不推导路径。服务端写、CLI 读，两个真实消费方。

## 决策七：外壳启动判定（宿主/附着）与关窗行为

**启动判定**（顺序固定，外壳的第一个动作不是拉起进程）：

```
读 run/polaris.port + run/polaris.token
  ├─ 文件缺失 ──────────────────────────► 核心是否存在于 DataLayout.Bin ？      
  │                                        ├─ 否 → 故障页「核心未安装」+ 可复制的安装命令
  │                                        └─ 是 → 【宿主模式】拉起，指数退避轮询至就绪
  └─ 文件存在 → 两段式探测
        ① GET /healthz          存活性
        ② GET <需鉴权的轻端点>   凭证有效性
        ├─ ①② 均通过 → 【附着模式】
        ├─ ① 通过 ② 401 → 故障页「凭证不匹配」，**不得拉起第二个实例**
        └─ ① 失败 → 陈旧 run/ 文件，按「文件缺失」分支处理
```

- **外壳不自己拼 run/ 路径**：它跑 `polaris service status --json`（2026-09-21 新增）向二进制询问端口、令牌、数据目录与日志目录。路径 SSoT 在 Go 侧的 `config.DataLayout`，Rust 再实现一遍必然漂移其一，且漂移时两边各自看起来都对；这同时解决了 `config.toml` 覆盖 `data_dir` 时外壳找不到 run/ 的问题。外壳里唯一硬编码的路径知识是二进制的默认安装位置。
- **两段式探测是必需的，不可省为只探 `/healthz`**：`middleware_auth.go` 的 `healthPathSet` 把 `/healthz` 列为免鉴权白名单，探通它只证明"有进程在听"，不证明"本外壳能调 API"。只探 healthz 会让外壳在凭证不匹配时进入附着模式，随后每个业务请求 401，表现为"界面全白但服务是好的"。
- 凭证不匹配的典型来源：守护进程由另一用户账号启动、或启动时设了 `POLARIS_API_KEY`。此时拉起第二个实例必被单实例锁拒绝（决策六 T2），故必须走故障页而非重试。
- **核心缺失**是可预期场景——用户把 `.app` / `.exe` 单独拷给别人时必然发生。外壳须在拉起子进程前检查 `<DataLayout.Bin>/polaris` 存在且可执行，缺失时显示故障页与一键复制的安装命令，**不得崩溃或静默退出**。
- 关窗 → 隐藏到托盘；**首次关窗弹一次系统通知**说明进程仍在运行，避免用户误以为已退出。
- 托盘退出分两项：「退出界面（守护进程继续运行）」为默认动作；「完全退出（同时停止守护进程）」**仅在宿主模式下出现**，附着模式下不提供——外壳没有资格停别人拉起的进程。停止用 SIGTERM 触发四阶优雅关停，30 秒未退出才强杀（Windows 无等价信号，直接终止）。
  > 2026-09-21 追记：初稿写的是偏好项 `quit_stops_daemon`（默认 false）。实施时改为两个显式菜单项——默认行为等价（不停），但无需在外壳里另建配置存储，且用户每次退出都能看见两种后果。
- **判活以单实例锁为准，不以 run/ 文件为准**（2026-09-21 实跑发现）：`kill -9` 后 run/ 文件残留而锁已被内核释放。按文件判活时，崩溃后点"重新连接"会走到"运行时状态异常"并要求用户手动删 run/。现由 `service status --json` 以 `probeInstanceLock` 判活，残留文件报 `stale: true` 且 `running: false`，外壳照常走宿主模式拉起。探测会以微秒级时长试探锁，故守护进程取锁改为约 1 秒的短暂重试，避免一次状态查询恰好撞上启动而令其拒绝启动。
- **巡检**：外壳后台每 5 秒探一次 `/healthz`，连接丢失/恢复均发系统通知并刷新托盘状态行；每 15 秒以本地令牌轮询 `/v1/approvals/pending`，新审批按 ID 去重后发通知。轮询走普通 HTTP，经过网关中间件，不是旁路 IPC（决策一）。理由：窗口收进托盘后 Web UI 不在跑，而 Agent 卡在待审批操作上恰恰多发生在用户没盯着界面时。
- 托盘图标反映守护进程状态（运行 / 附着 / 异常）；异常时提供重启与打开日志入口。
- 依据：定时任务、通道、HITL 审批是产品核心能力，"关窗即停"会使桌面版能力弱于网页版。

## 决策八：无证书分发——命令行安装为主渠道，不上架任何应用商店

**不上架 App Store / Microsoft Store**：macOS 沙箱与本项目核心机制逐条冲突——sidecar 子进程执行、Wasm/容器三级沙箱、`internal/vfs` 任意路径访问、本地端口监听、`purego` 加载 bundle 外 dylib；逐条申请 entitlement 成功率低且长期牵制架构演进，MSIX 容器有同类限制。

**主渠道是命令行安装脚本**（`curl -fsSL <url> | sh` / PowerShell），不是浏览器下载安装包。依据是各平台"未签名产物"的实际行为差异：

| 平台 | 浏览器下载安装包 | 命令行脚本安装 |
|---|---|---|
| macOS | Gatekeeper 拦截；macOS 15 起「右键打开」旁路已移除，须进系统设置手动放行 | **不拦**——隔离属性 `com.apple.quarantine` 由下载方（浏览器/邮件）写入，`curl` + `tar` 不写 |
| Windows | SmartScreen 警告；未签名产物无法积累信誉分，警告不会随时间消失 | `Invoke-WebRequest` 默认不写 MOTW，不触发 SmartScreen |
| Linux | 无签名要求 | 同左 |

- **macOS ad-hoc 签名是硬要求**：Apple Silicon 拒绝无签名的可执行文件与 dylib。`codesign -s -` 免费且不需要账号；Go 链接器对 darwin/arm64 产物自带 ad-hoc 签名。该约束在现有 tarball 渠道已经成立并被验证，桌面版不新增风险面。
- **信任由 cosign 承接**，不由平台签名承接：安装脚本在解压前校验 `.sha256` 及其签名（ADR-0095 锚点 A），用户亦可 `polaris release-key verify` 自验。这是本项目在无平台证书条件下的完整性保证，**不得因"反正没签名"而弱化**。
- 辅助渠道（均不需要开发者账号/证书）：Homebrew tap、Scoop bucket、AUR、Flathub。Homebrew cask 默认写隔离属性，安装脚本与文档须给出 `xattr -dr com.apple.quarantine` 的处置说明。
- **反例守护**：驳回"先把安装包做成双击即用的体验"——在无证书前提下这不可达；任何号称能消除 Gatekeeper/SmartScreen 警告而不购买证书的方案（自签根证书、绕过 MOTW 的下载器等），按本条驳回。

## 后果

- **正向**：三端能力天然对齐，无第二套 API；守护进程与 GUI 解耦，服务器部署与桌面部署共用同一二进制；零 CGO 与 5 平台交叉编译矩阵保持不变；桌面外壳可连远程实例。
- **负向**：CI 新增三 runner 的桌面打包链路与两套签名体系；决策五是破坏性变更，需 CHANGELOG 与迁移期；外壳需自行实现就绪等待与进程守护，这部分逻辑无法被现有 Go 门控覆盖。
- **反例守护**：见各决策条目末尾。

## 范围裁决（2026-09-21）

以下各项评估后**不做**，理由逐条记录，重提须带新事实：

| 项 | 裁决 | 理由 |
|---|---|---|
| 首启向导 | 不另做 | Web UI 已有 onboard 流程（`web/src/js/store/onboard.js`），外壳加载的就是它 |
| 远程连接地址簿 | 暂缓 | 远程实例的 Web UI 拿不到凭证：决策五只对回环对端下发令牌 Cookie，而这是刻意的。做地址簿之前须先设计远程场景的登录流程，否则得到的是一个连上即全屏 401 的功能 |
| 深链 `polaris://` | 暂缓 | 无具体使用场景。根 CLAUDE.md「禁止超前抽象、臆测开发」 |
| CLI TUI（Bubble Tea） | 暂缓 | 现有 REPL 已覆盖交互聊天；引入新依赖需具体需求支撑 |
| 外壳自更新（Tauri updater） | 暂缓 | 需更新清单与签名密钥管线；外壳本身极少变更，重跑安装脚本即可更新 |

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| Wails | 需 CGO（破坏交叉编译矩阵）+ 绑定开第二条业务通道 + 生命周期绑窗口；见决策二 |
| Lorca / webview_go | 依赖本机 Chrome 或引入 CGO；无托盘/通知/签名分发能力；维护状态差 |
| Electron | 体积与常驻内存与 [Tier-0] 2GB VPS 定位冲突 |
| 把 `web/dist` 打进外壳作静态资源 | 跨源 + 鉴权 + SSE 三处需另开机制，前端不再零改动；见决策三 |
| 把 Go 二进制作为 Tauri sidecar 打进 app bundle | 守护进程自更新与 bundle 完整性耦合；持证后就地替换会使签名失效，无证时 ad-hoc 签名同样被破坏；见决策四 |
| 以浏览器下载安装包为主渠道 | 无证书下 Gatekeeper / SmartScreen 必然拦截，而命令行安装本就不写隔离属性；见决策八 |
| 保留"回环即可信"以免破坏性变更 | 本机任意进程与浏览器页面可取得完整权限；见决策五 |
| 将 `internal/cli` 改造为客户端 SDK 层 | 偷换在册未接线包的语义，破坏 ADR-0088 台账可追溯性；且无第二个消费方，属超前抽象 |
| 上架 App Store / Microsoft Store | 沙箱禁止 sidecar 子进程、Wasm 运行时、任意文件访问与 bundle 外 dylib 加载 |

## 引用代码

- `cmd/polaris/cli.go`、`cmd/polaris/main.go`（CLI 客户端与命令分发）
- `cmd/polaris/boot_server.go`（守护进程装配、`updater.New` 落点）
- `internal/gateway/server/middleware_auth.go`（鉴权分支，决策五落点）
- `internal/gateway/server/server_init.go`（Web UI 静态资源挂载）
- `internal/sysmgr/updater/updater_install.go`（就地替换二进制，决策四事实依据）
- `internal/gateway/server/sysadmin/system_update.go`（`managed_by` 字段落点）
- `tools/baselines/wiring-allowlist.txt`（`internal/cli` 在册条目，决策六）
- `.github/workflows/release.yml`（`CGO_ENABLED=0` 交叉编译矩阵）
- `docs/arch/decisions/ADR-0095-updater-supply-chain-and-schema-downgrade-guard.md`（更新供应链信任模型）

## 重新评估触发条件

1. Wails 提供**不依赖 CGO** 的 macOS/Linux 构建路径，**且**有可复现用例证明其 AssetServer 能透传 SSE —— 可重提决策二。
2. Tauri updater 支持只替换 bundle 内 sidecar 且经 macOS 实测签名仍有效（`codesign --verify` 通过、Gatekeeper 放行）—— 可重议决策四的渠道划分。
3. 出现第二个 Go 语言的 API 客户端消费方（非测试）—— 可重议决策六"不抽 SDK"的结论。
4. 取得 Apple Developer ID 或 Windows 代码签名证书（含 SignPath Foundation 等面向开源项目的免费证书）—— 可重议决策八的渠道优先级；**决策四的 bundle 外布局仍应保留**，它在持证后同样成立且是自更新可用的前提。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-09-21 | 初稿：八条决策，对应桌面化/CLI 化的全部形态选择 |
| 2026-09-21 | 追记修订：补入「无开发者签名证书」这一约束。决策四由「桌面渠道禁用内置自更新」改为「sidecar 与 dylib 装在 bundle 之外」——初稿的前提（持有证书、bundle 需保持签名有效）不成立；决策八由「平台原生签名 + 不上架商店」改为「命令行安装为主渠道 + 不上架商店」，信任锚点全部落到 cosign。原结论的驳回理由见「被驳回的方案」表相应行。 |
| 2026-09-21 | 追记修订二：决策七扩写为启动判定状态机——两段式探测（`/healthz` 免鉴权，单探它无法判定凭证有效性）、核心缺失故障页、凭证不匹配不得拉起第二实例；决策三补「外壳内置纯静态故障页」这一唯一例外及其边界。 |
| 2026-09-21 | 追记修订三：状态转 Accepted；路径订正——守护进程安装位置写为 `DataLayout.Bin`（`~/.polarisagi/polaris/bin`，既有字段），run/ 三文件路径归入 DataLayout 而非由 runtimeinfo 自行推导，遵从 layout.go 的路径 SSoT 约定。 |
| 2026-09-21 | 实施追记：P0（T1~T7）与 P2 外壳骨架落地。三处与初稿的差异已并入正文——服务注册归并为单一实现（决策一）、`/healthz` 带版本号（决策四）、外壳经 `service status --json` 发现运行时状态（决策七）。 |
| 2026-09-21 | 实施追记二：外壳实跑七个分支（宿主/附着/凭证不匹配/崩溃检测/陈旧状态/核心缺失/版本不兼容）后并入正文——判活改以单实例锁为准、退出改为两个显式菜单项、新增后台巡检；新增「范围裁决」节记录不做的五项及理由。 |
| 2026-09-22 | 分发合并：GitHub Release 不再分"核心"和"桌面"两类产物。每平台/架构组合只出一个归档，内含守护进程 + Rust dylib + 桌面外壳（有桌面版的平台）+ configs。安装脚本从统一归档中提取桌面外壳并放到平台常规位置（macOS → `/Applications/`，Linux → `~/.local/bin/` + `.desktop` 文件，Windows → 开始菜单快捷方式）。决策四"分开安装"的语义收窄为"运行时分离"而非"分发渠道分离"——updater 仍只替换 `bin/` 与 `lib/`，不触碰桌面外壳。`tauri.conf.json` 移除 `dmg` 和 `deb` bundle targets；Windows 改用原始 exe 而非 `.msi`。 |
| 2026-09-22 | 分发合并补遗：卸载脚本（`uninstall.sh`/`uninstall.ps1`）与安装脚本同步补齐桌面外壳的清理/去重——此前只删 `bin/polaris`、`bin/lib`，桌面外壳落地后残留的解压原件和开始菜单快捷方式未清理。`tauri.conf.json` 的 `msi` bundle target 一并移除（release.yml 的 Windows 分支已改为直接拷贝原始 exe，不再消费 `.msi` 产物，遗留的 `msi` 目标只是白跑一次打包）；此项未经真实 CI 验证，待下次打 tag 触发 `desktop` job 确认 Tauri v2 在目标平台无可用 bundle target 时是否静默跳过而非报错，如报错则回退保留 `msi`。 |
