# ADR-0096: 桌面版/命令行版形态——守护进程 + 薄客户端，外壳用 Tauri v2

> 状态：Accepted（2026-09-21；守护进程契约化与外壳均已落地） ｜ 首次：2026-09-21 ｜ 最近复核：2026-10-12
> 模块：`cmd/polaris/`、`internal/gateway/server/`、`internal/sysmgr/updater/`、`internal/runtimeinfo/`、`desktop/`、`.github/workflows/release.yml`。`internal/cli` 已删除（决策六）。

约束事实：(1) CLI 已存在（`cmd/polaris/cli*.go` 全部经 HTTP 与本地服务通信，不直连数据库）；(2) 发布流水线以 `CGO_ENABLED=0` 单 runner 交叉编译 5 平台，GUI 框架不得编入主二进制；(3) 定时调度、通道、HITL 审批、OpenAI 兼容 API 要求进程在无窗口时继续运行。

## 决策一：三端共用唯一业务通道，外壳不得承载业务 IPC

客户端（Web / 桌面 / CLI / 第三方）与内核之间只有 `HTTP /v1/* + SSE` 一条路。

- 外壳 Rust 侧只做：子进程管理、窗口、托盘、系统通知、深链、外壳自更新。禁止任何业务语义的 Tauri command（command 清单即白名单，新增须先改本 ADR）。
- 桌面端能力是 CLI 能力的子集（同一组 API）。
- 反例：「为性能/便利在外壳里直连内核函数」——绕过 gateway 即绕过鉴权、限流、审计与 OTel（HE-1/HE-3/HE-7）。
- 同一规则适用于服务注册：`scripts/install.sh`/`install.ps1` 调用 `polaris service install`，单元文件只在 Go 一处生成（标签 `com.polarisagi.polaris`，Windows 任务名 `PolarisAGI-Polaris`）；脚本自写 plist/计划任务会产生两个注册项，第二个实例被单实例锁拒绝，表现为「服务时好时坏」。

## 决策二：外壳选 Tauri v2，否决 Wails；Go 二进制独立进程，外壳不链接内核

| 否决 Wails 的依据 | 后果 |
|---|---|
| macOS/Linux 构建需 CGO | 与 `CGO_ENABLED=0` 交叉编译矩阵冲突 |
| Go 方法自动绑定为前端调用 | 在 HTTP 外开第二条业务通道（违反决策一） |
| 改用 AssetServer 挂 gateway handler | SSE 流式支持存疑，而 `web/src/js/sse.js` 是主链路 |
| 框架进程即宿主进程 | 守护进程生命周期被窗口绑架 |

- 代价：Tauri 不支持交叉编译，CI 的 `desktop` job 走各平台 runner，与单 runner 交叉编译矩阵是并行两套逻辑，不得合并。不新增语言栈（Rust 已是一等公民）。

## 决策三：webview 加载 `http://127.0.0.1:<port>`，不把 `web/dist` 作为外壳静态资源

前端零改动的唯一路径：同源下相对 `fetch('/v1/...')`、`EventSource`、Cookie 鉴权原样工作。

- 代价：窗口须等待守护进程就绪——启动闪屏（`desktop/dist/boot.html`）+ 指数退避轮询（200ms 起、封顶 3s、总超时 60s）+ 超时错误窗口（含「打开日志」）。
- 唯一例外：外壳内置纯静态故障页（`desktop/dist/error.html`：核心未安装 / 启动超时 / 版本不兼容 / 凭证不匹配），不得调用任何 `/v1` 端点、不得含业务 UI，不得扩张为设置页。

## 决策四：sidecar 与 dylib 装在 app bundle 之外，外壳只是启动器

前提：项目没有 Apple Developer ID 与 Windows 代码签名证书，短期不购买。

| 组件 | 位置 | 更新者 |
|---|---|---|
| `polaris` 二进制 + `lib/`（含 `libsubstrate`） | `~/.polaris/bin/`（`config.DataLayout.Bin`，用户级） | `sysmgr/updater`，与 tarball 渠道一致（ADR-0095 信任模型） |
| 桌面外壳 | 平台常规位置（macOS `/Applications/`、Linux `~/.local/bin/` + `.desktop`、Windows 开始菜单） | 重跑安装脚本 |

- 外壳以固定路径拉起 `~/.polaris/bin/polaris`，不用 Tauri bundle 内 sidecar；`updater_install.go` 的就地替换（`os.Rename` 覆盖二进制与 `lib/`）永远发生在 bundle 之外，托管标记与 updater 拒绝安装分支无需实现。
- 外壳与守护进程版本解耦：`/healthz` 带版本号，版本不兼容时外壳提示而非崩溃。
- 分发：每平台/架构只出一个归档（守护进程 + Rust dylib + 桌面外壳 + configs），updater 仍只替换 `bin/` 与 `lib/`；`tauri.conf.json` bundle targets 为 `app`/`appimage`，Windows 用原始 exe。
- 与 ADR-0095 分工：0095 定校验值凭什么可信，本条定被替换文件放哪。反例：把 Go 二进制作为 Tauri sidecar 打进 bundle（自更新与 bundle 完整性耦合）。

## 决策五：本地令牌默认强制，回环豁免取消

原状：未配置 `POLARIS_API_KEY` 时「来自回环」即授予 `ClientTypeWebUI` 完整权限；桌面场景下本机任意进程与浏览器页面可取得完整权限，不可接受。

| 客户端 | 携带方式 |
|---|---|
| CLI / 桌面外壳 / 脚本 | `Authorization: Bearer` 或 `X-API-Key`，令牌读自 `run/polaris.token`（0600） |
| Web UI | gateway 返回 `index.html` 时下发 `Set-Cookie: polaris_local=<token>; HttpOnly; SameSite=Strict; Path=/` |
| 远程部署 | `POLARIS_API_KEY`（不变） |

- Cookie 使同源 `fetch`/`EventSource` 自动携带，前端不改；`HttpOnly` 阻断页面 JS，`SameSite=Strict` + Origin/Host 校验阻断 CSRF 与 DNS rebinding。
- 破坏性变更：逃生阀 `POLARIS_ALLOW_ANONYMOUS_LOOPBACK=1`（默认关）给本机裸调 `/v1/chat/completions` 的第三方客户端留迁移期。健康端点白名单（`/healthz` `/readyz` `/metrics` `/.well-known/agent-card.json`）豁免不变。
- `ClientType.IsLocalTrusted()` 的取值与令牌分支须一次对齐，不得留第二条隐式信任路径。无令牌的回环 POST 必须 401，由实跑用例锁死。
- 判定顺序：本地令牌校验先于 IP 冷却，远程 `POLARIS_API_KEY` 在冷却之后。理由：守护进程每次启动轮换令牌，旧外壳持旧令牌轮询触发的按 IP 冷却会把回环上持正确令牌的 CLI/新窗口一并锁死；本地令牌为随机 256 位，冷却对它无防护价值；持错误令牌的请求照常计失败。

## 决策六：`internal/cli` 删除；服务发现下沉为 `internal/runtimeinfo`

- `internal/cli` 是 wiring-allowlist 在册的未接线债务（`AgentREPL` 依赖 `InferFn` 直连内核，与决策一冲突）。已删包，条目在 `tools/baselines/wiring-allowlist.txt` 「已删除」段留记录。不改造为客户端 SDK（破坏 ADR-0088 台账可追溯性；无第二个 Go 消费方）；CLI 的 HTTP 客户端留在 `package main`。
- `internal/runtimeinfo` 只负责 `polaris.pid`/`polaris.port`/`polaris.token` 的原子写入、读取与权限校验；路径 SSoT 是 `internal/config.DataLayout`（`Run`/`RunPID`/`RunPort`/`RunToken` 字段），runtimeinfo 只接收路径、不推导。

## 决策七：外壳启动判定（宿主/附着）与关窗行为

```
读 run/polaris.port + run/polaris.token
  ├─ 缺失 → DataLayout.Bin 有核心？ 否 → 故障页「核心未安装」+ 可复制安装命令
  │                                 是 → 【宿主模式】拉起，指数退避轮询至就绪
  └─ 存在 → ① GET /healthz  ② GET <需鉴权轻端点>
        ├─ ①② 通过 → 【附着模式】
        ├─ ① 通过 ② 401 → 故障页「凭证不匹配」，不得拉起第二个实例
        └─ ① 失败 → 陈旧 run/，按「缺失」处理
```

- 外壳不自拼 run/ 路径：运行 `polaris service status --json` 询问端口、令牌、数据目录与日志目录（路径 SSoT 在 Go 侧，`config.toml` 覆盖 `data_dir` 时亦正确）；外壳唯一硬编码的路径知识是二进制默认安装位置。
- 两段式探测不可省：`/healthz` 在免鉴权白名单内，只证明有进程在听；只探它会在凭证不匹配时进入附着模式，随后业务请求全 401（「界面全白但服务是好的」）。凭证不匹配的典型来源：守护进程由另一用户启动或启动时设了 `POLARIS_API_KEY`；此时拉起第二实例必被单实例锁拒绝。
- 核心缺失（单独拷贝 `.app`/`.exe`）须显示故障页，不得崩溃或静默退出。
- 判活以单实例锁为准，不以 run/ 文件为准：`kill -9` 后文件残留而锁已释放。`service status --json` 以 `probeInstanceLock` 判活，残留文件报 `stale: true`、`running: false`；守护进程取锁带约 1 秒短重试，避免被探测撞上启动而拒绝启动。
- 关窗 → 隐藏到托盘，首次关窗弹一次系统通知。托盘退出两项：「退出界面」（默认，守护进程继续运行）；「完全退出」仅宿主模式出现（外壳无资格停别人拉起的进程），SIGTERM 触发四阶优雅关停，30 秒未退出才强杀（Windows 直接终止）。
- 巡检：每 5 秒探 `/healthz`，丢失/恢复发系统通知并刷新托盘状态行；每 15 秒以本地令牌轮询 `/v1/approvals/pending`，新审批按 ID 去重通知（走普通 HTTP，非旁路 IPC）。托盘图标反映运行 / 附着 / 异常，异常时提供重启与打开日志。
- 依据：定时任务、通道、HITL 审批是产品核心，「关窗即停」会使桌面版弱于网页版。

## 决策八：无证书分发——命令行安装为主渠道，不上架应用商店

- 不上架 App Store / Microsoft Store：沙箱与 sidecar 子进程、Wasm/容器沙箱、`internal/vfs` 任意路径、本地端口监听、`purego` 加载 bundle 外 dylib 逐条冲突。
- 主渠道是命令行安装脚本（`curl -fsSL <url> | sh` / PowerShell）：`curl`+`tar` 不写 macOS 隔离属性 `com.apple.quarantine`，`Invoke-WebRequest` 默认不写 Windows MOTW，故不触发 Gatekeeper / SmartScreen；浏览器下载安装包则必被拦截（macOS 15 起「右键打开」旁路已移除，未签名产物无法积累 SmartScreen 信誉）。
- macOS ad-hoc 签名是硬要求（Apple Silicon 拒绝无签名可执行文件与 dylib；`codesign -s -` 免费，Go 链接器对 darwin/arm64 自带）。
- 信任由 cosign 承接：安装脚本解压前校验 `.sha256` 及其签名（ADR-0095 锚点 A），用户可 `polaris release-key verify` 自验；不得因「反正没签名」弱化。
- 辅助渠道（规划，仓内尚无打包定义）：Homebrew tap、Scoop bucket、AUR、Flathub；Homebrew cask 默认写隔离属性，文档须给出 `xattr -dr com.apple.quarantine` 处置。
- 反例：「先把安装包做成双击即用」；任何不买证书却宣称能消除 Gatekeeper/SmartScreen 警告的方案（自签根证书、绕过 MOTW 的下载器）。

## 范围裁决（不做项，重提须带新事实）

| 项 | 裁决 | 理由 |
|---|---|---|
| 首启向导 | 不做 | Web UI 已有 onboard 流程（`web/src/js/store/onboard.js`） |
| 远程连接地址簿 | 暂缓 | 决策五只对回环对端下发令牌 Cookie，远程 Web UI 无凭证；须先设计远程登录流程 |
| 深链 `polaris://` | 暂缓 | 无具体场景（禁止超前抽象） |
| CLI TUI（Bubble Tea） | 暂缓 | 现有 REPL 已覆盖；新依赖需具体需求 |
| 外壳自更新（Tauri updater） | 暂缓 | 需更新清单与签名密钥管线；重跑安装脚本即可 |

## 已驳方案

| 方案 | 驳回理由 |
|---|---|
| Wails / Lorca / webview_go | CGO、第二业务通道、生命周期绑窗口，或依赖本机 Chrome 且无托盘/通知（决策二） |
| Electron | 体积与常驻内存与 Tier-0 2GB VPS 定位冲突 |
| `web/dist` 打进外壳 | 跨源、鉴权、SSE 三处需另开机制（决策三） |
| Go 二进制作为 Tauri sidecar 打进 bundle | 自更新与 bundle 完整性耦合（决策四） |
| 浏览器下载安装包为主渠道 | 无证书下必被拦截（决策八） |
| 保留「回环即可信」 | 本机任意进程与浏览器页面取得完整权限（决策五） |
| `internal/cli` 改造为客户端 SDK | 偷换未接线包语义，无第二消费方（决策六） |

## 重新评估触发条件

1. Wails 提供不依赖 CGO 的 macOS/Linux 构建且有可复现用例证明 AssetServer 透传 SSE → 重提决策二。
2. Tauri updater 支持只替换 bundle 内 sidecar 且 macOS 实测签名仍有效（`codesign --verify` 通过、Gatekeeper 放行）→ 重议决策四渠道划分。
3. 出现第二个 Go API 客户端消费方（非测试）→ 重议决策六不抽 SDK。
4. 取得 Apple Developer ID 或 Windows 签名证书（含 SignPath Foundation 等开源免费证书）→ 重议决策八渠道优先级；决策四的 bundle 外布局仍保留。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-09-21 | 初稿八条决策；补入「无签名证书」约束：决策四由「禁用内置自更新」改为「sidecar 与 dylib 装在 bundle 外」，决策八由「平台签名 + 不上架」改为「命令行安装为主渠道」，信任锚点落到 cosign；决策七扩为启动判定状态机；转 Accepted；服务注册归并单一实现；判活改以单实例锁、退出改两个显式菜单项（取代偏好项 `quit_stops_daemon`）；新增范围裁决 |
| 2026-09-22 | 分发合并为每平台单一归档；决策四「分开安装」收窄为运行时分离；卸载脚本同步清理桌面外壳；移除 `dmg`/`deb`/`msi` bundle targets |
| 2026-09-25 | 决策五：本地令牌校验先于 IP 冷却 |
| 2026-10-12 | 压缩；辅助渠道标注为规划（仓内无打包定义），bundle targets、外壳模块、故障页状态以现码为准 |

## 引用代码

`cmd/polaris/{cli,cli_service,main,boot_server,boot_runtime}.go`、`internal/gateway/server/{middleware_auth,server_init,server_handlers}.go`、`internal/gateway/server/sysadmin/system_update.go`、`internal/sysmgr/updater/updater_install.go`、`internal/runtimeinfo/`、`internal/config/layout.go`、`desktop/src-tauri/src/`、`desktop/src-tauri/tauri.conf.json`、`desktop/dist/`、`scripts/{install,uninstall}.{sh,ps1}`、`tools/baselines/wiring-allowlist.txt`、`.github/workflows/release.yml`、`docs/arch/decisions/ADR-0095-updater-supply-chain-and-schema-downgrade-guard.md`
