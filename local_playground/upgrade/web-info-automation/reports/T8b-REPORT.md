# T8b 汇报：Polaris Browser Bridge 浏览器扩展与本地 CDP 中继

## 完成范围

- **① 浏览器扩展（`plugins/browser_use/extension/`）**：
  - MV3 标准扩展，纯原生 JS 开发，无任何外部编译或构建依赖；
  - 权限声明严格遵循最小化原则：仅申请 `debugger`, `tabs`, `storage`, `alarms`，**不申请 host_permissions、不注入 content scripts、不读取或修改 cookies**；
  - 固定扩展 ID：生成 2048 位 RSA 密钥，计算 DER 摘要映射生成固定扩展 ID `ccdonkdehfibjcejkfjoicdklfdffjeh` 并配置公钥至 manifest `key`，私钥 `key.pem` 忽略并不入库；
  - 自动发现与握手：扩展通过 `chrome.alarms` 与启动监听自动扫描 `127.0.0.1:18790-18799` 端口，尝试建立 WebSocket 连接，发送配对令牌完成握手并保持心跳保活；
  - 严格窗口与标签隔离：扩展仅对自身通过 `chrome.windows.create` 打开的独立非聚焦窗口及所属标签调用 `chrome.debugger.attach`，对其他非自有标签的命令直接拦截拒绝；
  - 选项页（`options.html`/`options.js`）：提供简洁的配对令牌粘贴保存与多端中继连接状态展示。
- **② 本地双口中继服务（`bridge_relay.py`）**：
  - 扩展服务端口（18790-18799）：严格校验 WebSocket 请求头 `Origin == chrome-extension://ccdonkdehfibjcejkfjoicdklfdffjeh`（非法来源直接返回 403 Forbidden），握手令牌基于 `hmac.compare_digest` 安全常量时间比对（错误令牌以 4401 关闭）；
  - Playwright CDP 服务端口（随机端口）：提供 HTTP `GET /json/version` 标准版本接口，伪装为本地 Chrome CDP 浏览器端点；
  - CDP 双向协议转换：
    - `Browser.getVersion` / `Browser.setDownloadBehavior` / `Target.getTargetInfo` / `Target.getBrowserContexts` 本地应答；
    - `Target.setAutoAttach`：触发扩展创建初始空白标签并向 Playwright 发送 `Target.attachedToTarget` 事件，固定关联会话 `pw-tab-1`；
    - 标签级命令（`sessionId="pw-tab-1"`）：剥离 sessionId 后作为轻量 RPC 转发给扩展；
    - 子目标级命令（iframe/worker）：保留子会话 sessionId 原样转发；
    - 事件与分离转发：扩展上报事件自动补齐 sessionId，扩展分离或标签关闭通知转化为 `Target.detachedFromTarget` 下发给 Playwright。
- **③ ExtensionBackend 接入与 BrowserHub 单标签隔离模式**：
  - `ExtensionBackend`：遵循 Backend 协议，在线时 `unattended_ok() -> True`，支持免弹窗无人值守运行；
  - 智能后端裁决：`auto` 模式优先等待扩展中继握手（最多等待 2s），显式指定时等待 30s；扩展未就绪时抛出友好提示引导用户在扩展选项页粘贴令牌；
  - 单标签安全约束：扩展模式下 `new_owned_page` 自动复用当前工作页并返回友好说明，`owned_pages` 与 `status` 仅暴露当前工作标签，杜绝混淆；
  - 全局并发锁保护：为 `BrowserHub` 引入 `self.page_lock` 互斥锁，对所有页面交互工具（`open`, `read`, `snapshot`, `act`, `scroll`, `extract`, `tabs`）与管线 `run_harvest` 进行串行化保护，彻底防止并发踩踏；
  - 空闲自动释放：扩展模式默认 10 分钟空闲自动断开 CDP 连接与释放标签，消除浏览器顶部的调试提示条。
- **④ 文档与配对流程**：
  - 更新 `plugins/browser_use/README.md`，增加「安装扩展（推荐，一次性，支持免弹窗无人值守）」的图文步骤；
  - 更新 `plugins/browser_use/skills/browser_use/SKILL.md`，增加 `attach_failed` 错误码的对用户引导话术；
  - `browser_status` 工具返回字段扩充 `extension_dir`、`bridge_token`、`bridge_token_path` 与 `pair_instruction`。
- **⑤ 跨插件一致性**：
  - 同步复制更新后的 `browser_hub.py`、`bridge_relay.py`、`bridge_const.py` 至 `plugins/social_poster/src/utils/`，并通过 `check.sh` step 6 的 SHA-256 强校验。

---

## 提交列表

仓库 `polaris-plugins-official`：
1. `8d8ac94` `feat(browser_use): 交付 Polaris Browser Bridge 浏览器扩展与常量`
2. `98d6ca2` `feat(browser_use): 交付 CDP 双口中继服务与 ExtensionBackend 接入`
3. `6c7e391` `docs(browser_use): 交付浏览器扩展安装说明与配对文档`

---

## 交付证据

### 1. 全量代码规范与合规检查 (`check.sh`)
```bash
$ ./check.sh -p plugins/browser_use -s
══════════════════════════════════════════
  Python 项目检测 — 2026-10-14 19:19:50
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

### 2. 单元测试套件全部通过 (`uv run pytest -q`)
```bash
$ uv run pytest -q
...............                                                          [100%]
15 passed in 2.57s
```
测试覆盖：
- `test_relay_origin_check`：非法 Origin 返回 403 Forbidden，非法路径返回 404；
- `test_relay_token_auth`：错误令牌以 4401 关闭连接，正确令牌成功握手并标记就绪；
- `test_relay_json_version`：HTTP `/json/version` 返回合规协议版本与 WebSocket 端点；
- `test_relay_cdp_routing_lifecycle`：CDP 指令分发、`pw-tab-1` sessionId 剥离、事件会话补齐与 detached 生命周期流转；
- `test_harvest.py` 与 `test_ratelimit.py` 现有套件 100% 保持通过。
