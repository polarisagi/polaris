# ADR-0122: Shell 与路径静态分析——单一分析器、敏感路径目录化、原文兜底扫描

- **状态**: Accepted
- **日期**: 2026-10-21
- **决策者**: MrLaoLiAI（设计：Claude；实现：Sonnet）
- **相关模块**: M11（`internal/security/policy`、删除 `internal/security/classifier`）/ M07（bash、run_command）/ M06（hooks exec）/ Rust `native_sandbox`
- **修订**: ADR-0121 决策四（敏感路径清单扩充为目录文件）；ADR-0117 决策四（`cmdclass` 为唯一 shell 分类器）

## 上下文

ADR-0121 后完全访问下 shell 宿主执行，防线全在 `Decide` 的静态分析。现状缺口（代码事实）：

1. **两套 shell 分析器**：`policy/cmdclass`（mvdan/sh 语法树，供 Decide）与 `security/classifier`（正则 DENY/HITL/WARN，供 bash/run_command 工具内与 hooks exec）。规则口径不一（`rm -rf x` 一边 Unknown 一边 HITL），正则 HITL 在 Decide 之外另起审批通道。
2. **敏感路径只有 5 项**（`~/.ssh ~/.gnupg ~/.aws ~/.netrc ~/Library/Keychains`）：浏览器配置目录（Cookies、Login Data、Local Storage 令牌、钱包扩展）、密码管理器、云 CLI 凭据、Windows DPAPI/凭据管理器、加密钱包、IM 会话、持久化落点（shell rc、LaunchAgents、systemd user、`.git/hooks`、其他 agent 配置）均未覆盖。
3. **大小写绕过**：APFS/NTFS 默认大小写不敏感，前缀匹配却区分大小写，`~/library/keychains` 可绕过。
4. **破坏性命令判级偏低**：`rm -rf ./*`（cwd 为项目根）、`rm -rf .git`、`rm -rf "$X/"*`、`git reset --hard`、`git clean -fdx`、`diskutil eraseDisk` 为 Unknown/Dangerous——full_access 下静默执行，不可恢复。
5. **运行期展开不可见**：`cat $(echo ~/.polaris/vault.key)`、`echo … | base64 -d | sh` 语法树判不出目标。

参考（只借鉴清单与判据，不引入依赖）：Dicklesworthstone/destructive_command_guard（Claude Code PreToolUse 破坏性命令拦截：git reset --hard / checkout -- / clean -f、rm -rf 非临时目录、"数据 vs 执行"上下文判定）、anthropic-ai/sandbox-runtime（强制拒写 `.bashrc/.zshrc/.gitconfig/.git/hooks/.mcp.json`，不可被 allowWrite 覆盖）、AlessandroZ/LaZagne（各平台浏览器/密码管理器/凭据存储落点）、MITRE ATT&CK T1555（密码存储）/ T1552.001（文件中的凭据）/ T1546.004（shell 配置持久化）/ T1543（系统服务持久化）、Falco `sensitive_files` 宏。

## 决策

**结论：shell 分析只有 `policy` 一处——语法树为主判据，原文正则扫描为兜底；路径分类改为按平台的内嵌目录文件；破坏不可恢复数据的命令一律硬限制。**

### 决策一：单一 shell 分析器

删除 `internal/security/classifier`。其 DENY 规则迁入 `cmdclass`（判 `CmdCatastrophic` → 硬限制询问），HITL/WARN 规则删除（与 Decide 重复）。bash/run_command/hooks exec 不再自带分级：

- 有授权凭据（Grant≠None）：Decide 已判定，工具内不再分级。
- 无凭据（fail-closed 兜底）：调用 `policy.ClassifyCommand`，`≥ CmdDangerous` 拒绝。
- hooks exec（第三方 hook 命令，无 Decide）：`CmdCatastrophic` 拒绝（等同原 RiskDeny 语义）。

### 决策二：路径目录文件 `policy/pathcatalog/*.yaml`（go:embed）

按 `os: all|darwin|linux|windows` 分节，条目支持 `~`、`%APPDATA%`/`%LOCALAPPDATA%`/`%USERPROFILE%`/`%PROGRAMDATA%`、单段 `*` 通配。类别与判定：

| 类别 | 读 | 写 | 典型 |
|---|---|---|---|
| `self`（不在 yaml，由 DataLayout 生成） | 拒绝 | 拒绝 | vault.key、config.toml、secrets/、data/、audit/、eval/、KILLSWITCH |
| `credential` | 硬限制 | 硬限制 | SSH/GPG、云 CLI、浏览器配置目录、密码管理器、OS 钥匙串/DPAPI、钱包、IM 会话、shell 历史 |
| `persistence` | 放行 | 硬限制 | shell rc、`.gitconfig`、LaunchAgents/Daemons、systemd user、autostart、Startup 文件夹、`~/.polaris/{hooks,extensions,skills}`、其他 agent 配置（`~/.claude ~/.codex ~/.gemini ~/.cursor`）、**任何位置**的 `.git/hooks/`、`.git/config` |
| `system` | 放行 | 硬限制 | `/etc /usr /bin /sbin /lib /boot /System /Library /private/etc`、`C:\Windows`、`C:\Program Files*`、`C:\ProgramData` |
| `basename`（仅可信根外） | 硬限制 | 硬限制 | `id_rsa* id_ed25519* id_ecdsa* *.kdbx *.kdb *.keychain-db wallet.dat *.p12 *.pfx *.ppk` |

- **匹配一律大小写不敏感**于 darwin/windows（Linux 区分）；原路径与 EvalSymlinks 真实路径任一命中即算。
- `credential`/`self` 进入沙箱 `DenyPaths`（读写拒）；`persistence`/`system` 进入新增的 `DenyWritePaths`（Seatbelt `deny file-write*`、bwrap 已绑定目录内 ro-bind 覆盖）。
- 目录文件的改动须附 `pathcatalog` 用例（每条至少一正例）。

### 决策三：破坏性命令升级为灾难性（硬限制）

判据是"不可恢复地毁掉用户数据或安全姿态"：

- 删除类（rm/rmdir/shred/unlink/trash、`find … -delete|-exec rm`）目标为：根、家目录、系统顶层目录（现有）；家目录一级用户数据目录（Documents/Desktop/Downloads/Pictures/Movies/Music/Library/.config/.local）；**任一可信根本身或其 `/*`**（含 cwd 为可信根时的 `.`、`./*`、`*`）；任何 `.git` 目录；**以运行期展开开头**的递归删除目标（`$X/…`、`${X}…`、`$(…)…`，空变量即 `rm -rf /*`）。
- git：`reset --hard`、`clean -f*`、`checkout -- .`/`checkout .`/`restore .`（整树丢弃）、`stash drop|clear`、对 `main|master|trunk|release*` 或未写明分支的 `push --force`。其余 force push、`branch -D` 保持 Dangerous。
- 磁盘：`dd of=/dev/*`、`mkfs*`（现有）；`diskutil eraseDisk|eraseVolume|zeroDisk|secureErase|partitionDisk`、`wipefs`、写入块设备的重定向。
- 安全姿态：`ufw disable`、`iptables -F`、`nft flush ruleset`、`pfctl -d`、`netsh advfirewall … off`、`csrutil disable`、`spctl --master-disable`、`useradd/usermod/dscl` 加入 sudo/wheel/admin 组、`passwd root`、`insmod/modprobe/kextload`、`nc -l` 监听、`chroot`。
- `chmod -R`/`chown -R` 作用于根、家目录、系统目录。

### 决策四：原文兜底扫描（正则，确定性但可绕过）

在语法树分类之外，对**完整命令原文**（含引号、`$(…)`、heredoc）做一次扫描，命中即 `CmdCatastrophic`（硬限制询问，**不作为拒绝依据**）：

1. **敏感片段**：由目录文件派生——自保护路径相对数据根的片段（如 `.polaris/vault.key`、`.polaris/secrets`）及绝对路径、`credential` 类的特征片段（`Login Data`、`key4.db`、`logins.json`、`.aws/credentials`、`Keychains`、`.kdbx` 等，目录文件里以 `fragments:` 声明）。
2. **混淆执行**：解码/下载输出管道进解释器（`base64 -d|--decode`、`xxd -r`、`openssl enc -d`、`curl`/`wget` → `sh|bash|zsh|python*|perl|ruby|node`）；`eval` 或 `sh|bash -c` 的参数含运行期展开；fork bomb。
3. **内嵌脚本破坏**：`python -c`/`node -e`/`perl -e`/`ruby -e` 代码串含 `rmtree|os.remove|unlink|rmSync|rm -rf` 且同时含家目录/根路径片段。

"数据 vs 执行"：片段只出现在 `grep|rg|ag|echo|printf` 的模式参数里、且该段无管道进解释器时不命中（借鉴 dcg）。

### 决策五：门控

- `internal/security/policy/testdata/cmdclass_corpus.tsv`：`命令<TAB>cwd<TAB>期望等级<TAB>原因码`，表驱动测试；含本 ADR 每条判据的正例与易误报反例（`rm -rf build/`、`git checkout -b x`、`grep -r "rm -rf" .`、`cat ~/.zshrc`）。
- 路径目录用例 `pathcatalog_test.go`：每条目录项正例 + 大小写变体 + 软链。

## 后果

- **正向**: 一个分析器、一套口径；凭据与持久化落点覆盖三大平台主流软件；大小写绕过关闭；`rm -rf ./*` 类误操作在完全访问下也会问一次。
- **负向**: 完全访问下 `git reset --hard`、`git clean -fdx` 每次都询问（不可记住），频繁使用的用户会感到打扰——这是"不可恢复数据损失"的刻意代价；原文扫描可被字符串拼接/编码绕过，仍是尽力而为，物理保证只在沙箱（`DenyPaths`）与 hardened 档；浏览器整目录归 credential，读书签也会询问。
- **反例守护**: 原文扫描命中不得升级为拒绝（误报只应多问一次）；不得重新引入第二个 shell 分类器；新增敏感软件只改目录文件，不在代码里散写路径。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 只扩充正则规则库（沿用 classifier） | 正则不理解引号/管道/cwd，`grep "rm -rf"` 误报、`rm -rf "$D"/*` 漏报；与语法树双轨口径不一 |
| 原文扫描命中直接拒绝 | 可绕过的判据不能做拒绝边界（HE-2），且误报会让合法命令永远跑不了 |
| 对浏览器只列具体文件（Login Data、Cookies） | 版本间文件名变动、扩展存储（钱包）分散在子目录，整目录更稳；代价是读书签也询问 |
| 引入 dcg 作为依赖 | 外部 Rust 二进制 + hook 通道，与 Decide 并行判定违背单一授权源 |

## 引用代码

`internal/security/policy/{cmdclass*.go,protected_paths.go,pathcatalog/,approval_ops.go}`、`internal/tool/builtin/{bash,run_command}`、`internal/action/hook/{runner,exec_command}.go`、`internal/tool/sandbox/rust_native_sandbox.go`、`rust/substrate/src/native_sandbox/{seatbelt,bwrap,types}.rs`
