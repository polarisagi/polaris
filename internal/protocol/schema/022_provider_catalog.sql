-- ============================================================================
-- 022_provider_catalog: 厂商字典 + 模型字典（只读种子，简化用户配置）
-- ============================================================================
-- 设计意图：用户选择 sys_providers 条目、输入 API Key，系统自动填充
--   provider type/base_url 并从 sys_provider_models 生成带角色的模型配置。
-- 角色分配规则（由后端 from-catalog 接口执行）：
--   default   → capability_tier='smart' AND is_reasoning=0，按 display_order 取第一个
--   reasoning → is_reasoning=1，按 display_order 取第一个
--   general   → 其余全部
-- 关联: M1(Inference Runtime), M13(Interface)
-- ============================================================================

-- 厂商字典（只读，系统内置）
CREATE TABLE IF NOT EXISTS sys_providers (
    id               TEXT PRIMARY KEY,                               -- 英文小写标识，与 hermes 对齐
    display_name     TEXT NOT NULL,
    provider_type    TEXT NOT NULL CHECK(provider_type IN ('openai_compat','anthropic','google_agent_platform','ollama')),
    default_base_url TEXT NOT NULL DEFAULT '',                       -- openai_compat 必填；anthropic/google 为空（SDK 硬编码）
    is_local         INTEGER NOT NULL DEFAULT 0,                     -- 1=本地无需 API Key
    display_order    INTEGER NOT NULL DEFAULT 0
);

-- 模型字典（只读，系统内置）
CREATE TABLE IF NOT EXISTS sys_provider_models (
    id                 TEXT PRIMARY KEY,                             -- '{provider_id}:{model_id}'
    catalog_provider_id TEXT NOT NULL REFERENCES sys_providers(id),
    model_id           TEXT NOT NULL,                               -- 发给 API 的原始 model 名
    display_name       TEXT NOT NULL,
    capability_tier    TEXT NOT NULL CHECK(capability_tier IN ('smart','fast')),
    is_reasoning       INTEGER NOT NULL DEFAULT 0,                  -- 1=专用推理/思考模型
    display_order      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_sys_prov_models_provider ON sys_provider_models(catalog_provider_id);

-- ============================================================
-- 厂商种子数据（15 个主流厂商，覆盖国内外）
-- ============================================================

INSERT OR IGNORE INTO sys_providers (id, display_name, provider_type, default_base_url, is_local, display_order) VALUES
('deepseek',  'DeepSeek (深度求索)',      'openai_compat',          'https://api.deepseek.com',                                  0,  1),
('anthropic', 'Anthropic',               'anthropic',              '',                                                          0,  2),
('openai',    'OpenAI',                  'openai_compat',          'https://api.openai.com',                                    0,  3),
('google',    'Google AI Studio',        'google_agent_platform',  '',                                                          0,  4),
('moonshot',  '月之暗面 (Kimi)',           'openai_compat',          'https://api.moonshot.cn/v1',                                0,  5),
('dashscope', '阿里云 (通义千问)',          'openai_compat',          'https://dashscope.aliyuncs.com/compatible-mode/v1',         0,  6),
('doubao',    '火山引擎 (豆包)',            'openai_compat',          'https://ark.cn-beijing.volces.com/api/v3',                  0,  7),
('zhipu',     '智谱 AI (GLM)',            'openai_compat',          'https://open.bigmodel.cn/api/paas/v4',                      0,  8),
('qianfan',   '百度千帆 (文心)',            'openai_compat',          'https://qianfan.baidubce.com/v2',                           0,  9),
('xai',       'xAI (Grok)',              'openai_compat',          'https://api.x.ai/v1',                                       0, 10),
('mistral',   'Mistral AI',             'openai_compat',          'https://api.mistral.ai/v1',                                 0, 11),
('minimax',   'MiniMax (海螺)',           'openai_compat',          'https://api.minimax.chat/v1',                               0, 12),
('hunyuan',   '腾讯混元',                 'openai_compat',          'https://api.hunyuan.cloud.tencent.com/v1',                  0, 13),
('stepfun',   '阶跃星辰 (StepFun)',        'openai_compat',          'https://api.stepfun.com/v1',                                0, 14),
('ollama',    'Ollama (本地部署)',         'ollama',                 'http://127.0.0.1:11434',                                    1, 15);

-- ============================================================
-- 模型种子数据（每厂商 2-4 个主力模型）
-- display_order: 0=旗舰 1=推理 2=速度型
-- ============================================================

INSERT OR IGNORE INTO sys_provider_models (id, catalog_provider_id, model_id, display_name, capability_tier, is_reasoning, display_order) VALUES
-- ── DeepSeek ────────────────────────────────────────────────
('deepseek:deepseek-v4-pro',    'deepseek',  'deepseek-v4-pro',    'DeepSeek V4 Pro',     'smart', 0, 0),
('deepseek:deepseek-v4-flash',  'deepseek',  'deepseek-v4-flash',  'DeepSeek V4 Flash',   'fast',  0, 2),
('deepseek:deepseek-reasoner',  'deepseek',  'deepseek-reasoner',  'DeepSeek Reasoner',   'smart', 1, 1),
('deepseek:deepseek-chat',      'deepseek',  'deepseek-chat',      'DeepSeek Chat',       'smart', 0, 3),
-- ── Anthropic ───────────────────────────────────────────────
('anthropic:claude-opus-4-8',   'anthropic', 'claude-opus-4-8',    'Claude Opus 4.8',     'smart', 0, 0),
('anthropic:claude-sonnet-4-6', 'anthropic', 'claude-sonnet-4-6',  'Claude Sonnet 4.6',   'smart', 0, 1),
('anthropic:claude-haiku-4-5',  'anthropic', 'claude-haiku-4-5',   'Claude Haiku 4.5',    'fast',  0, 2),
-- ── OpenAI ──────────────────────────────────────────────────
('openai:gpt-4o',               'openai',    'gpt-4o',             'GPT-4o',              'smart', 0, 0),
('openai:gpt-4o-mini',          'openai',    'gpt-4o-mini',        'GPT-4o Mini',         'fast',  0, 2),
('openai:o4-mini',              'openai',    'o4-mini',            'o4 Mini',             'smart', 1, 1),
('openai:o3',                   'openai',    'o3',                 'o3',                  'smart', 1, 3),
-- ── Google AI Studio ────────────────────────────────────────
('google:gemini-2.5-pro',       'google',    'gemini-2.5-pro',     'Gemini 2.5 Pro',      'smart', 0, 0),
('google:gemini-2.5-flash',     'google',    'gemini-2.5-flash',   'Gemini 2.5 Flash',    'fast',  0, 1),
('google:gemini-2.0-flash',     'google',    'gemini-2.0-flash',   'Gemini 2.0 Flash',    'fast',  0, 2),
-- ── 月之暗面 (Kimi) ──────────────────────────────────────────
('moonshot:kimi-k2.6',          'moonshot',  'kimi-k2.6',          'Kimi K2.6',           'smart', 0, 0),
('moonshot:kimi-k2-thinking',   'moonshot',  'kimi-k2-thinking',   'Kimi K2 Thinking',    'smart', 1, 1),
-- ── 通义千问 (DashScope) ─────────────────────────────────────
('dashscope:qwen-3-max',        'dashscope', 'qwen-3-max',         'Qwen 3 Max',          'smart', 0, 0),
('dashscope:qwen3.6-flash',     'dashscope', 'qwen3.6-flash',      'Qwen 3.6 Flash',      'fast',  0, 2),
('dashscope:qwen3-max-thinking','dashscope', 'qwen3-max-thinking', 'Qwen 3 Max Thinking', 'smart', 1, 1),
-- ── 豆包 ────────────────────────────────────────────────────
('doubao:doubao-1.5-pro',       'doubao',    'doubao-1.5-pro',     'Doubao 1.5 Pro',      'smart', 0, 0),
('doubao:doubao-1.5-lite',      'doubao',    'doubao-1.5-lite',    'Doubao 1.5 Lite',     'fast',  0, 1),
-- ── 智谱 ────────────────────────────────────────────────────
('zhipu:glm-5.1',               'zhipu',     'glm-5.1',            'GLM-5.1',             'smart', 0, 0),
('zhipu:glm-5-turbo',           'zhipu',     'glm-5-turbo',        'GLM-5 Turbo',         'fast',  0, 1),
-- ── 百度千帆 ────────────────────────────────────────────────
('qianfan:ernie-4.5-300b-a47b', 'qianfan',   'ernie-4.5-300b-a47b','ERNIE 4.5 300B',      'smart', 0, 0),
('qianfan:ernie-4.5-21b-a3b',   'qianfan',   'ernie-4.5-21b-a3b',  'ERNIE 4.5 21B',       'fast',  0, 1),
-- ── xAI ─────────────────────────────────────────────────────
('xai:grok-4.20',               'xai',       'grok-4.20',          'Grok 4.20',           'smart', 0, 0),
('xai:grok-4.3',                'xai',       'grok-4.3',           'Grok 4.3',            'fast',  0, 1),
-- ── Mistral ─────────────────────────────────────────────────
('mistral:mistral-large-2512',  'mistral',   'mistral-large-2512', 'Mistral Large 2512',  'smart', 0, 0),
('mistral:mistral-small-2603',  'mistral',   'mistral-small-2603', 'Mistral Small 2603',  'fast',  0, 1),
-- ── MiniMax ─────────────────────────────────────────────────
('minimax:minimax-m2.7',        'minimax',   'minimax-m2.7',       'MiniMax M2.7',        'smart', 0, 0),
('minimax:minimax-m2.5',        'minimax',   'minimax-m2.5',       'MiniMax M2.5',        'fast',  0, 1),
-- ── 腾讯混元 ────────────────────────────────────────────────
('hunyuan:hunyuan-a13b-instruct','hunyuan',  'hunyuan-a13b-instruct','Hunyuan A13B',       'smart', 0, 0),
-- ── 阶跃星辰 ────────────────────────────────────────────────
('stepfun:step-3.5-flash',      'stepfun',   'step-3.5-flash',     'Step 3.5 Flash',      'fast',  0, 0);
-- Ollama 无预设模型（依赖用户本地 pull），通过 from-catalog 创建 provider 后手动添加模型
