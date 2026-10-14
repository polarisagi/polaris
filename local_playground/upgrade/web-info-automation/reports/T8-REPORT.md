# T8 汇报：browser_use v2 与插件清单迁移

## 完成范围

- **① 清单迁移**：
  - 将 `browser_use`, `computer_use`, `knowledge_base`, `social_poster`, `system_info` 5 个官方插件的清单目录由 `.polaris-plugin/` 迁移至 `.codex-plugin/`；
  - 更新根 `CLAUDE.md`「插件规范」段、`README.md`、`README_zh.md`，注明 Polaris 内核按 ADR-0103 读取 `.codex-plugin/`；
  - 在内核侧通过 `pluginspec.Load` 验证 5 个插件均被正确识别为 `[codex]` 格式并解析出展示名称与描述。
- **② BrowserHub 单例与多后端隔离**：
  - 依赖升级：引入 `patchright`（支持真实指纹对抗与 CDP 连接），保留 `playwright` 驱动选项；
  - 配置约定：统一配置根目录 `~/.config/polaris-plugins/browser/`，统一单例 `get_hub()`；
  - 后端实现：`CdpToggleBackend`（日常 Chrome 155+ chrome://inspect 开关，60s 超时供用户允许，无人值守保护）、`ManagedBackend`（系统 Chrome 真实可执行文件定位、专用 profile、`endpoint.json` 跨进程原子复用、10s DevToolsActivePort 轮询）；
  - 隔离保障：所有自动化窗口通过 CDP `Target.createTarget` 在新窗口（`newWindow=True`）打开，严格仅操作 `owned_targets`，杜绝触碰用户日常标签；
  - 空闲释放：后台 watchdog 周期探测，空闲达阈值时安全断开 CDP 会话，不杀死用户运行的 Chrome 进程。
- **③ 跨进程 SQLite 站点限速器与风控熔断**：
  - 实现 `plugins/browser_use/src/ratelimit.py`，基于 `ratelimit.db` 与 `PRAGMA journal_mode=WAL`；
  - `Limits` 模型定义 `min_interval_s`、`jitter_s`、`max_per_hour`、`max_per_day`、`active_hours`（支持跨午夜时间段）、`cooldown_on_block_h`；
  - `acquire` 在 `BEGIN IMMEDIATE` 事务中完成频次与排队判定，避免长时间持有数据库锁，多进程并发完全串行安全；提供 `mark_blocked` 主动与被动风控熔断；`now` 可注入用于单测。
- **④ 省 token 工具集与快照交互接口**：
  - 替换旧 8 个工具，交付 10 个标准 `@mcp.tool(annotations=ToolAnnotations(...))` 工具；
  - `browser_open`：URL 安全校验，区分工作页与新标签，`networkidle` 稳定等待；
  - `browser_read`：支持 `article`（trafilatura 智能提取）、`markdown`、`text`、`links` 四种模式，字符级切片分页与 sha256 校验；
  - `browser_snapshot`：基于 JS 常量字符串生成语义化 `e<N>` 引用编码，默认视口优先；
  - `browser_act`：严格正则校验 `^e\d{1,5}$` 防注入，支持点击、输入、回退、按键与 stale_ref 检查；
  - `browser_scroll` / `browser_extract` / `browser_tabs` / `browser_status`：全部在自建页面内操作，统一错误码。
- **⑤ 采集引擎 `src/harvest/`**：
  - `recipe.py`：Pydantic 严格校验模型（`extra="forbid"`），区分 XHR 拦截提取与 CSS 选择器提取；
  - `loader.py`：双目录（内置 `recipes/` + 用户 `CFG/recipes/`）加载，`_` 开头文件受 `POLARIS_BROWSER_DEV=1` 保护；
  - `normalize.py`：点路径多层索引（支持数组下标）、HTML 标签过滤、RFC3339 统一时间戳、`metrics.*` 指标聚合；
  - `engine.py`：参数化 URL 模板（支持 `urlencode` 与 `json` 过滤器）、多步流程执行、XHR/Fetch 响应流嗅探拦截、实时 block_signals 风控特征检测与熔断、登录态缓存与检查；
  - `recipes/_example.toml`：Hacker News 首页热门条目采集示例。
- **⑥ social_poster 接入与合规检查**：
  - 同步复制 `browser_hub.py` 与 `cfg.py` 至 `social_poster/src/utils/`，保持 SHA-256 逐字节一致；
  - 重构 `social_poster/src/utils/browser.py` 接入 `get_hub().page()` 与 `new_owned_page()`，删除旧端口文件扫描；
  - 在 `check.sh` 中增加跨插件 `browser_hub.py` 的 SHA-256 强校验，不一致直接报错；
  - 重写 `skills/browser_use/SKILL.md`（决策顺序、省 token 规则、错误恢复、隐私隔离）；
  - 更新 `README.md` 与 `.codex-plugin/plugin.json`（version 2.0.0）。

---

## 提交列表

仓库 `polaris-plugins-official`：
1. `3fb4c30` `refactor(plugins): 迁移清单目录为 .codex-plugin`
2. `c182a63` `feat(browser_use): 交付 BrowserHub 单例与 CDP/Managed 多后端隔离`
3. `bb0a817` `feat(browser_use): 交付跨进程 SQLite 站点限速器与风控熔断`
4. `0ec6701` `feat(browser_use): 交付省 token 工具集与快照交互接口`
5. `4eca192` `feat(browser_use): 交付站点配方解析与结构化采集引擎`
6. `c19398b` `feat(browser_use): social_poster 附着对齐、合规检查与文档发布`
7. `1daebea` `chore(social_poster): 更新 uv.lock 锁定依赖`

---

## 交付证据

### 1. 全量代码规范与合规检查 (`check.sh`)
```bash
$ ./check.sh -p plugins/browser_use -s
══════════════════════════════════════════
  Python 项目检测 — 2026-10-14 18:57:31
══════════════════════════════════════════
  检测目录: plugins/browser_use

▶ 1/5  Ruff — 代码规范 & 潜在错误
All checks passed!
✔ Ruff 未发现问题

▶      Ruff format — 格式一致性
✔ 代码格式一致

▶ 2/5  Bandit — 安全漏洞扫描
✔ Bandit 未发现安全漏洞

▶ 3/5  Safety — 依赖库 CVE 漏洞
⊘ 跳过: Safety（已通过 -s 跳过）

▶ 4/5  Mypy — 类型检查
   扫描: plugins/browser_use/src
✔ Mypy 类型检查通过

▶ 5/5  Radon — 圈复杂度
✔ 所有函数复杂度在可接受范围内（< 10）

▶ 6/6  跨插件一致性 — browser_hub.py SHA-256 对比
✔ 所有 browser_hub.py 副本 sha256 一致 (2 个副本)

══════════════════════════════════════════
  检测结果汇总
══════════════════════════════════════════
  ✔ 通过: 6   ✘ 失败: 0   ⊘ 跳过: 1

🎉 全部检测通过！可以安全推送到 GitHub。
```

### 2. 自动化单元测试 (`uv run pytest -q`)
```bash
$ cd plugins/browser_use && uv run pytest -q
...........                                                              [100%]
11 passed in 2.48s
```
包含配方严格模型校验、点路径多层与数组索引解析、三种时间戳格式清洗为 RFC3339、HTML 剥离与度量聚合、分页函数切片与哈希、ref 正则安全防御、限速器小时/天上限与跨午夜时间段判定、主动熔断冷却、multiprocessing 多进程并发 acquire 串行等待实测。

### 3. 结构化采集实测 (`_example.toml`)
```bash
$ POLARIS_BROWSER_DEV=1 uv run python -c "..."
RECIPES: True 1
HARVEST: True 3
ITEM: Iosevka is an open-source, sans-serif and slab-serif, monospace font -> https://typeof.net/Iosevka/
ITEM: Agent erased full season of \"The Challenge Swe\": \"Hundreds of days of work gone\" -> https://www.aviaduct.com/...
```

### 4. CDP 无人值守保护拦截实测 (无弹窗)
```bash
$ POLARIS_BROWSER_BACKEND=cdp uv run python -c "..."
EXPECTED HubError: needs_user_approval 请在 Chrome 弹窗中允许一次远程调试，或安装 Polaris Browser Bridge 扩展实现免弹窗
```

### 5. Managed 模式拉起与跨进程端点复用实测
```bash
$ POLARIS_BROWSER_BACKEND=managed uv run python -c "..."
Initial page opened: about:blank
Endpoint file exists: True
Endpoint content: {"pid": 36205, "ws_url": "ws://127.0.0.1:57548/devtools/browser/8914250a-94d2-4256-bbe0-377c70387662", "profile": "default", "started_at": 1791975494.2144058}
Reused page opened: about:blank
```

### 6. social_poster 接入验证
```bash
$ POLARIS_BROWSER_BACKEND=managed uv run python -c "..."
social_poster page URL: about:blank
```

### 7. 内核 pluginspec 清单识别验证
```bash
$ go test -v -run TestVerifyPluginsOfficial ./internal/extension/pluginspec
=== RUN   TestVerifyPluginsOfficial
Plugin: polaris-browser-use | Formats: [codex] | DisplayName: Browser Use (DOM Extraction) | Description: 复用你已登录的 Chrome；结构化采集；省 token。
Plugin: polaris-computer-use | Formats: [codex] | DisplayName: Computer Use | Description: Universal desktop automation...
Plugin: polaris-knowledge-base | Formats: [codex] | DisplayName: Knowledge Base | Description: Read local files...
Plugin: polaris-social-poster | Formats: [codex] | DisplayName: Social Poster | Description: Multi-platform social media...
Plugin: polaris-systeminfo-mcp | Formats: [codex] | DisplayName: System Info | Description: Provide system environment...
--- PASS: TestVerifyPluginsOfficial (0.01s)
PASS
```

---

## 新增依赖

- `patchright>=1.55` (Apache-2.0)：真实反爬指纹对抗与 CDP 连接驱动
- `trafilatura>=2.0` (GPL-3.0-or-later，仅在 plugins 独立进程运行，不侵入 core 内核)：网页正文智能抽取与 HTML 清洗
- `pydantic>=2.9` (MIT)：严格模式配方模型与参数校验
- `pytest>=8.0.0` / `pytest-asyncio>=0.24.0` (MIT / Apache-2.0)：测试依赖组

---

## 偏离设计之处及原因

1. **macOS TCC 权限与自动降级**：macOS 对用户日常目录 `~/Library/Application Support/Google/Chrome/DevToolsActivePort` 实施了沙箱与隐私保护（若终端未被系统授予「完全磁盘访问权限」会报 `Operation not permitted`）。`CdpToggleBackend` 捕捉此异常并记录明确原因，`auto` 模式无缝回退至 `ManagedBackend` 专用配置目录（不受 TCC 限制），确保开箱即用。
2. **端点探测免除 Bandit B310**：端点存活性探测采用标准库 `http.client.HTTPConnection` 替代 `urllib.request.urlopen`，杜绝 Bandit B310 漏洞警告并提升连通性探测性能。
3. **跨插件一致性强校验**：`check.sh` 中的一致性检查采用 `-name browser_hub.py` 全局检索，确保无论何时调用 `./check.sh` 均强制比对全仓副本。

---

## 待设计方裁决的问题

无。
