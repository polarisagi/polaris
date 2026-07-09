-- ============================================================================
-- 006_decision_log: 审计决策日志
-- ============================================================================
-- 架构角色: 记录所有架构级决策的日志。独立于 events 表的操作日志，
--           专注于可审计的"系统做了哪个选择"而非"系统发生了什么"。
--           与 M11 AuditTrail 互补 —— AuditTrail 记录安全事件，本表记录推理决策。
-- 生产者:    M1 ProviderRouter（路由决策）、M4 Agent Kernel（状态转移决策）、
--           M6 Skill Library（技能检索决策）、M9 Self-Improve（自进化决策）、
--           M8 Orchestrator（编排决策）、M5 Memory（Consolidation 触发决策）、
--           M13 TrafficSplitter（流量分发决策）
-- 消费者:    M12 Eval Harness（回归分析——对比决策变化与成功率）、
--           M3 Observability（DecisionLog.Analyze 统计分析）、
--           M11 AuditTrail（安全审计时关联查询）
-- 不变量:
--   1. append-only, 不可删除 [HE-Rule-6]
--   2. outcome 异步更新 —— 先写入决策（choice），后续补充结果（outcome）
--   3. 覆盖 13 种决策类型（见 decision_type 注释）
-- 写入路径: MutationBus（所有决策日志统一入口）
-- 关联模块: M3(Observability) §10, M12(Eval) §2, M11(Policy) §7
-- ============================================================================

CREATE TABLE IF NOT EXISTS decision_log (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    -- ↑ 自增主键。

    timestamp     INTEGER NOT NULL,
    -- ↑ 决策时间（Unix 毫秒）。

    session_id    TEXT NOT NULL,
    -- ↑ 所属会话 ID。

    agent_id      TEXT NOT NULL,
    -- ↑ 做出决策的 Agent ID。

    decision_type TEXT NOT NULL,
    -- ↑ 决策类型——共 13 种:
    --   'route_model'             —— M1 ProviderRouter 模型路由选择
    --   'select_tool'             —— M4/M7 工具选择
    --   'skill_lookup'            —— M6 SkillRetriever 技能检索
    --   'memory_retrieve'         —— M5 HybridRetriever 记忆检索
    --   'state_transition'        —— M4 FSM 状态转移
    --   'budget_limit'            —— M3 TokenBurnRate 熔断触发
    --   'consolidation_trigger'   —— M5 Consolidation 触发
    --   'logic_collapse_trigger'  —— M6 Logic Collapse 触发
    --   'auto_curriculum_generate'—— M9 Auto-Curriculum 课程生成
    --   'memf_prune'              —— M9 MEMF 剪枝决策
    --   'persona_refine'          —— M9 PersonaRefiner 画像更新
    --   'progressive_rollout'     —— M9 ProgressiveRollout 流量推进
    --   'co_evolution_compensate' —— M9 CoEvolutionCoordinator 联合进化补偿

    context       JSON,
    -- ↑ 决策上下文（JSON）。包含影响决策的关键输入信息。

    choice        TEXT NOT NULL,
    -- ↑ 决策结果。如: "model:deepseek-v4-flash" | "skill:go-concurrency-review@v1.2" |
    --   "state:S_PERCEIVE→S_PLAN" | "prune:3_events_by_similarity>0.9"。

    alternatives  JSON,
    -- ↑ 候选选项列表（JSON）。如: ["gpt-5.x","claude-sonnet-4.6","deepseek-v4-flash"]。
    --   用于 M12 Eval 分析"为什么选了 X 而不是 Y"。

    reason        TEXT,
    -- ↑ 决策理由（自然语言）。如: "预算限制下成本最优且 P95 延迟满足要求"。

    outcome       JSON
    -- ↑ 决策结果（异步更新）。如: {"success": true, "latency_ms": 230, "token_cost": 0.015}。
    --   用于 M12 Eval 计算每种决策类型的成功率。
);

-- 按会话 + 时间检索（M12 Eval 回归分析按 session 回放决策链）
CREATE INDEX IF NOT EXISTS idx_decision_session
    ON decision_log(session_id, timestamp);

-- 按决策类型 + 时间统计（M3 DecisionLog.Analyze 按类型统计成功率）
CREATE INDEX IF NOT EXISTS idx_decision_type
    ON decision_log(decision_type, timestamp);
