-- ============================================================================
-- 022_provider_catalog: 厂商字典 + 模型字典（只读种子，简化用户配置）
-- ============================================================================
-- 设计意图：用户选择 sys_providers 条目、输入 API Key，系统自动填充
--   provider type/base_url 并从 sys_provider_models 生成带角色的模型配置。
--
-- 角色分配规则（recommended_role 直接写入 provider_models.role）：
--   default   → 日常对话首选（快速响应，普通对话/工具调用）
--   reasoning → 深度推理（慢但强，复杂规划/代码分析/Agent）
--   general   → 通用候补池（仅三模型厂商启用；两模型厂商不设此角色）
--
-- 两模型厂商（fast+pro 模式）：default + reasoning，路由器自动 fallback。
-- 三模型厂商（Anthropic/OpenAI/DashScope）：default + reasoning + general。
-- 无推理模型厂商（Google/Hunyuan/StepFun）：default + general（reasoning 路由
--   fallback 到 default，由 ProviderRegistry.BestForRole 保证）。
--
-- 关联: M1(Inference Runtime), M13(Interface)
-- ============================================================================

-- 厂商字典（只读，系统内置）
CREATE TABLE IF NOT EXISTS sys_providers (
    id               TEXT PRIMARY KEY,                               -- 英文小写标识
    display_name     TEXT NOT NULL,
    provider_type    TEXT NOT NULL CHECK(provider_type IN ('openai_compat','anthropic','google_agent_platform','ollama')),
    default_base_url TEXT NOT NULL DEFAULT '',                       -- openai_compat 必填；anthropic/google 为空（SDK 硬编码）
    is_local         INTEGER NOT NULL DEFAULT 0,                     -- 1=本地无需 API Key
    display_order    INTEGER NOT NULL DEFAULT 0
);

-- 模型字典（只读，系统内置）
-- recommended_role 直接映射到运行时 provider_models.role，from-catalog 时零翻译复制。
CREATE TABLE IF NOT EXISTS sys_provider_models (
    id                  TEXT PRIMARY KEY,                            -- '{provider_id}:{model_id}'
    catalog_provider_id TEXT NOT NULL REFERENCES sys_providers(id),
    model_id            TEXT NOT NULL,                               -- 发给 API 的原始 model 名
    display_name        TEXT NOT NULL,
    recommended_role    TEXT NOT NULL DEFAULT 'general'
                            CHECK(recommended_role IN ('default','reasoning','general')),
    display_order       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_sys_prov_models_provider ON sys_provider_models(catalog_provider_id);

-- ============================================================
-- 厂商种子数据（15 个主流厂商，覆盖国内外）
-- ============================================================

INSERT OR IGNORE INTO sys_providers (id, display_name, provider_type, default_base_url, is_local, display_order) VALUES
('deepseek',  'DeepSeek (深度求索)',    'openai_compat',         'https://api.deepseek.com',                                  0,  1),
('anthropic', 'Anthropic',             'anthropic',             '',                                                          0,  2),
('openai',    'OpenAI',                'openai_compat',         'https://api.openai.com',                                    0,  3),
('google',    'Google AI Studio',      'google_agent_platform', '',                                                          0,  4),
('moonshot',  '月之暗面 (Kimi)',         'openai_compat',         'https://api.moonshot.cn/v1',                                0,  5),
('dashscope', '阿里云 (通义千问)',        'openai_compat',         'https://dashscope.aliyuncs.com/compatible-mode/v1',         0,  6),
('doubao',    '火山引擎 (豆包)',          'openai_compat',         'https://ark.cn-beijing.volces.com/api/v3',                  0,  7),
('zhipu',     '智谱 AI (GLM)',          'openai_compat',         'https://open.bigmodel.cn/api/paas/v4',                      0,  8),
('qianfan',   '百度千帆 (文心)',          'openai_compat',         'https://qianfan.baidubce.com/v2',                           0,  9),
('xai',       'xAI (Grok)',            'openai_compat',         'https://api.x.ai/v1',                                       0, 10),
('mistral',   'Mistral AI',            'openai_compat',         'https://api.mistral.ai/v1',                                 0, 11),
('minimax',   'MiniMax (海螺)',         'openai_compat',         'https://api.minimax.chat/v1',                               0, 12),
('hunyuan',   '腾讯混元',               'openai_compat',         'https://api.hunyuan.cloud.tencent.com/v1',                  0, 13),
('stepfun',   '阶跃星辰 (StepFun)',      'openai_compat',         'https://api.stepfun.com/v1',                                0, 14),
('ollama',    'Ollama (本地部署)',       'ollama',                'http://127.0.0.1:11434',                                    1, 15);

-- ============================================================
-- 模型种子数据（精简至最新主力模型，按两模型/三模型规则分配角色）
-- display_order: 0=default 1=reasoning 2=general
-- ============================================================

INSERT OR IGNORE INTO sys_provider_models (id, catalog_provider_id, model_id, display_name, recommended_role, display_order) VALUES

-- ── DeepSeek（两模型：flash=对话，pro=推理）────────────────────────────────
-- 官方推荐模型，旧版 deepseek-chat/deepseek-reasoner 别名已停用
('deepseek:deepseek-v4-flash',    'deepseek',  'deepseek-v4-flash',    'DeepSeek V4 Flash',   'default',   0),
('deepseek:deepseek-v4-pro',      'deepseek',  'deepseek-v4-pro',      'DeepSeek V4 Pro',     'reasoning', 1),

-- ── Anthropic（三模型：haiku=通用，sonnet=对话，opus=推理）────────────────
('anthropic:claude-haiku-4-5',    'anthropic', 'claude-haiku-4-5',     'Claude Haiku 4.5',    'general',   2),
('anthropic:claude-sonnet-4-6',   'anthropic', 'claude-sonnet-4-6',    'Claude Sonnet 4.6',   'default',   0),
('anthropic:claude-opus-4-8',     'anthropic', 'claude-opus-4-8',      'Claude Opus 4.8',     'reasoning', 1),

-- ── OpenAI（三模型：mini=通用，gpt-5.5=对话，o3=推理）───────────────────
('openai:gpt-5.4-mini',           'openai',    'gpt-5.4-mini',         'GPT-5.4 Mini',        'general',   2),
('openai:gpt-5.5',                'openai',    'gpt-5.5',              'GPT-5.5',             'default',   0),
('openai:o3',                     'openai',    'o3',                   'o3',                  'reasoning', 1),

-- ── Google AI Studio（无专用推理模型：pro=对话，flash=通用）────────────────
-- reasoning 路由自动 fallback 到 default（BestForRole 保证）
('google:gemini-3.1-pro',         'google',    'gemini-3.1-pro',       'Gemini 3.1 Pro',      'default',   0),
('google:gemini-3.5-flash',       'google',    'gemini-3.5-flash',     'Gemini 3.5 Flash',    'general',   1),

-- ── 月之暗面 Kimi（两模型：k2.6=对话，k2-thinking=推理）──────────────────
('moonshot:kimi-k2.6',            'moonshot',  'kimi-k2.6',            'Kimi K2.6',           'default',   0),
('moonshot:kimi-k2-thinking',     'moonshot',  'kimi-k2-thinking',     'Kimi K2 Thinking',    'reasoning', 1),

-- ── 阿里云通义千问（三模型：flash=通用，max=对话，max-thinking=推理）────────
('dashscope:qwen3.6-flash',       'dashscope', 'qwen3.6-flash',        'Qwen 3.6 Flash',      'general',   2),
('dashscope:qwen-3-max',          'dashscope', 'qwen-3-max',           'Qwen 3 Max',          'default',   0),
('dashscope:qwen3-max-thinking',  'dashscope', 'qwen3-max-thinking',   'Qwen 3 Max Thinking', 'reasoning', 1),

-- ── 火山引擎豆包（两模型：lite=对话，pro=推理）───────────────────────────
('doubao:doubao-1.5-lite',        'doubao',    'doubao-1.5-lite',      'Doubao 1.5 Lite',     'default',   0),
('doubao:doubao-1.5-pro',         'doubao',    'doubao-1.5-pro',       'Doubao 1.5 Pro',      'reasoning', 1),

-- ── 智谱 GLM（两模型：turbo=对话，glm-5.1=推理）──────────────────────────
('zhipu:glm-5-turbo',             'zhipu',     'glm-5-turbo',          'GLM-5 Turbo',         'default',   0),
('zhipu:glm-5.1',                 'zhipu',     'glm-5.1',              'GLM-5.1',             'reasoning', 1),

-- ── 百度千帆 ERNIE（两模型：21b=对话，300b=推理）─────────────────────────
('qianfan:ernie-4.5-21b-a3b',     'qianfan',   'ernie-4.5-21b-a3b',    'ERNIE 4.5 21B',       'default',   0),
('qianfan:ernie-4.5-300b-a47b',   'qianfan',   'ernie-4.5-300b-a47b',  'ERNIE 4.5 300B',      'reasoning', 1),

-- ── xAI Grok（两模型：grok-4.3=对话，grok-4.20=推理）────────────────────
('xai:grok-4.3',                  'xai',       'grok-4.3',             'Grok 4.3',            'default',   0),
('xai:grok-4.20',                 'xai',       'grok-4.20',            'Grok 4.20',           'reasoning', 1),

-- ── Mistral（两模型：small=对话，large=推理）──────────────────────────────
('mistral:mistral-small-2603',    'mistral',   'mistral-small-2603',   'Mistral Small 2603',  'default',   0),
('mistral:mistral-large-2512',    'mistral',   'mistral-large-2512',   'Mistral Large 2512',  'reasoning', 1),

-- ── MiniMax（两模型：m2.5=对话，m2.7=推理）───────────────────────────────
('minimax:minimax-m2.5',          'minimax',   'minimax-m2.5',         'MiniMax M2.5',        'default',   0),
('minimax:minimax-m2.7',          'minimax',   'minimax-m2.7',         'MiniMax M2.7',        'reasoning', 1),

-- ── 腾讯混元（单模型：reasoning fallback 到 default）────────────────────
('hunyuan:hunyuan-a13b-instruct', 'hunyuan',   'hunyuan-a13b-instruct','Hunyuan A13B',        'default',   0),

-- ── 阶跃星辰（单模型）────────────────────────────────────────────────────
('stepfun:step-3.5-flash',        'stepfun',   'step-3.5-flash',       'Step 3.5 Flash',      'default',   0);

-- Ollama 无预设模型（依赖用户本地 pull），from-catalog 后手动在设置页添加模型
