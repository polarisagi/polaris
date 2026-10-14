# T9 汇报：站点配方（知乎 / 小红书 / X / 微博）与 harvest_selftest 工具交付

## 完成范围

- **① 4 大主流平台结构化采集配方（`plugins/browser_use/recipes/*.toml`）**：
  - **知乎 (`zhihu.toml`)**：
    - `hot`：全网热榜动作，XHR 拦截 `*/api/v3/feed/topstory/hot-lists/total*`，字段映射 `target.{id, url, title, excerpt, author.name}` 及 `detail_text`（热度）；配置 `.HotList-item` CSS 降级提取。
    - `search`：问答与文章搜索动作，支持参数 `q`，XHR 拦截 `*/api/v4/search_v3*`，映射 `object.{id, url, excerpt, author.name, voteup_count}` 与 `highlight.title`；配置 `div.SearchResult-Card` CSS 降级提取。
  - **小红书 (`xiaohongshu.toml`)**：
    - `search`：图文/笔记搜索动作，支持参数 `q` 及排序选项 `sort` (`general` / `time_descending`)，XHR 拦截 `*/api/sns/web/v1/search/notes*`，映射 `id`、`note_card.{display_title, user.nickname, interact_info.liked_count}`；配置 `section.note-item` CSS 降级提取。
  - **X / Twitter (`x.toml`)**：
    - `search`：推文实时搜索动作，支持参数 `q` 及模式选项 `mode` (`top` / `latest`)，XHR 拦截 `*/graphql/*/SearchTimeline*`，映射推文 ID、全文、发帖人 screen_name、发布时间及点赞/转发数；配置 `article[data-testid='tweet']` CSS 降级提取。
    - `user_timeline`：用户推文时间线动作，支持参数 `handle`，XHR 拦截 `*/graphql/*/UserTweets*`，映射推文结构及发布时间；配置相同 CSS 降级提取。
  - **微博 (`weibo.toml`)**：
    - `hot`：实时热搜榜动作，XHR 拦截 `*/ajax/statuses/hot_band*`，映射话题词、分类、导语及热度值；配置 `div.wbpro-feed-content` CSS 降级提取。
    - `search`：博文搜索动作，支持参数 `q`，XHR 拦截 `*/ajax/statuses/search*`，映射微博 mid、正文文本、发布用户、发布时间及转评赞互动指标；配置 `div.card-wrap` CSS 降级提取。
- **② 保守频次限速与风控熔断契约**：
  - 各站配置严格限速：知乎（≥4s, ≤60/h）、小红书（≥8s, ≤30/h）、X（≥5s, ≤50/h）、微博（≥5s, ≤50/h）；
  - 配置各站风控状态码（403/429/461）与特征 URL 片段（captcha, unhuman, passport/visitor 等）及提示文案；触发风控后自动冷却 6~12 小时；所有配方头部标注 `status = "beta"`。
- **③ 自测工具 `harvest_selftest` 交付**：
  - 在 `src/tools_harvest.py` 交付 `_calc_field_rates` 与 `tool_harvest_selftest(site)`；
  - 在 `src/main.py` 正式注册为只读 MCP 工具 `@mcp.tool(annotations=ToolAnnotations(readOnlyHint=True)) harvest_selftest(site: str)`；
  - 对目标站点的每个动作执行最小调用并统计核心字段填充率（id, url, title, author, published_at, excerpt, metrics）。
- **④ 样本夹具与单元测试**：
  - 新建 `tests/fixtures/{zhihu,xiaohongshu,x,weibo}/` 真实数据样本（严格去除个人信息、Token、Cookie 等隐私标识）；
  - 新建 `tests/test_site_recipes.py`，覆盖配方解析、限速与风控信号校验、多站点 XHR 数据抽取与 `normalize_items` 标准化清洗、CSS fallback 契约、`_calc_field_rates` 与 `tool_harvest_recipes`。
- **⑤ SKILL.md 文档增补**：
  - 增补第 5 节《结构化采集配方 (harvest) 与合规准则》，系统阐述 4 大站点动作、参数、限速、状态探测工具及数据合规红线。

---

## 提交列表

仓库 `polaris-plugins-official`：
- `2d0cfc3` `feat(browser_use): 交付知乎/小红书/X/微博站点配方与 harvest_selftest 工具`

---

## 交付证据

### 1. 规范检查 (`check.sh`)
```bash
$ ./check.sh -p plugins/browser_use -s
══════════════════════════════════════════
  Python 项目检测 — 2026-10-14 19:31:14
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

### 2. 单元测试与配方验证 (`pytest`)
```bash
$ uv run --directory plugins/browser_use pytest
============================= test session starts ==============================
platform darwin -- Python 3.14.6, pytest-9.1.1, pluggy-1.6.0
rootdir: /Users/mrlaoliai/PolarisAGI/polaris-plugins-official/plugins/browser_use
configfile: pyproject.toml
plugins: asyncio-1.4.0, anyio-4.13.0
asyncio: mode=Mode.AUTO, debug=False, asyncio_default_fixture_loop_scope=None, asyncio_default_test_loop_scope=function
collecting ... collected 23 items

tests/test_bridge_relay.py ....                                          [ 17%]
tests/test_harvest.py ........                                           [ 52%]
tests/test_ratelimit.py ...                                              [ 65%]
tests/test_site_recipes.py ........                                      [100%]

============================== 23 passed in 3.42s ==============================
```
