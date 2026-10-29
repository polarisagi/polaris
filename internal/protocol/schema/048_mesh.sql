-- 048_mesh: 跨节点委派（ADR-0111）——对端令牌账本 + 入站委派任务账本。
-- 架构角色: internal/gateway/mesh 是两张表的唯一写入点。
--
-- mesh_peer_tokens：本节点签发给对端的访问令牌。只存 SHA-256 摘要，明文只在签发时出现一次
--   （与 GitHub PAT 同一做法）：库文件泄露不等于令牌泄露。令牌与 POLARIS_API_KEY 完全分离，
--   中间件把持有者身份定为 ClientTypePeer，只能访问 /a2a/v1、/mcp 与 Agent Card（ADR-0111 决策三）。
--   吊销 = 置 revoked_at，不删行：审计需要知道某个已吊销令牌曾属于谁。
CREATE TABLE IF NOT EXISTS mesh_peer_tokens (
    token_id     TEXT    PRIMARY KEY,              -- 公开 ID（列表/吊销用），不是令牌本身
    token_hash   TEXT    NOT NULL UNIQUE,          -- hex(SHA-256(明文令牌))
    peer_name    TEXT    NOT NULL,                 -- 本节点给对端起的名字（日志/审计可读）
    peer_node_id TEXT    NOT NULL,                 -- 对端节点 ID；外部非 Polaris Agent 为 ext:<name>
    scopes       TEXT    NOT NULL,                 -- 逗号分隔：a2a:send,a2a:read,a2a:cancel
    created_at   INTEGER NOT NULL,                 -- Unix 毫秒
    expires_at   INTEGER NOT NULL,                 -- Unix 毫秒；签发时强制有限期
    revoked_at   INTEGER NOT NULL DEFAULT 0,       -- 0 = 未吊销
    last_used_at INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX IF NOT EXISTS idx_mesh_peer_tokens_peer ON mesh_peer_tokens(peer_node_id);

-- mesh_peer_tasks：对端经 A2A 或 MCP 门面提交的委派任务。
--   task_id 一律由本节点生成（调用方的 messageId 只作幂等键）——此前私有端点直接拿调用方
--   JSON-RPC id 当黑板主键，撞键时被 INSERT OR IGNORE 静默丢弃却仍回 accepted。
--   结果在本表留存到 expires_at（默认 7 天，ADR-0111 决策七），与黑板终态任务 24h 清理解耦：
--   对端机器可能休眠数天，醒来后仍要能取回结果。
--   被拒绝的委派（深度超限/成环/超大输入）也落一行 state=rejected、bb_task_id 为空，留审计痕迹。
CREATE TABLE IF NOT EXISTS mesh_peer_tasks (
    task_id        TEXT    PRIMARY KEY,
    bb_task_id     TEXT    NOT NULL DEFAULT '',    -- 黑板任务 ID；rejected 行为空
    caller_node_id TEXT    NOT NULL,
    caller_name    TEXT    NOT NULL,
    token_id       TEXT    NOT NULL DEFAULT '',
    protocol       TEXT    NOT NULL CHECK(protocol IN ('a2a','mcp')),
    context_id     TEXT    NOT NULL,
    message_id     TEXT    NOT NULL DEFAULT '',    -- 调用方幂等键，同一调用方内唯一
    target_agent   TEXT    NOT NULL DEFAULT '',
    spawn_depth    INTEGER NOT NULL,
    chain          TEXT    NOT NULL,               -- JSON 数组：经过的节点 ID（含调用方，不含本节点）
    input_text     TEXT    NOT NULL,
    state          TEXT    NOT NULL CHECK(state IN
                     ('submitted','working','input_required','completed','failed','canceled','rejected')),
    status_message TEXT    NOT NULL DEFAULT '',
    result         TEXT    NOT NULL DEFAULT '',
    canceled       INTEGER NOT NULL DEFAULT 0,     -- 取消请求已发出（黑板侧取消后统一表现为 failed）
    created_at     INTEGER NOT NULL,               -- Unix 毫秒
    updated_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_mesh_peer_tasks_msg
    ON mesh_peer_tasks(caller_node_id, message_id) WHERE message_id != '';
CREATE INDEX IF NOT EXISTS idx_mesh_peer_tasks_caller ON mesh_peer_tasks(caller_node_id, created_at);
CREATE INDEX IF NOT EXISTS idx_mesh_peer_tasks_bb ON mesh_peer_tasks(bb_task_id);
CREATE INDEX IF NOT EXISTS idx_mesh_peer_tasks_expiry ON mesh_peer_tasks(expires_at);

-- mesh_peers：本节点登记的对端（出站方向）。由 `polaris peer accept` 写入，State-in-DB 而非配置文件：
--   配对需要程序化写入（固定公钥、出站令牌），改用户手写的 TOML 极易破坏格式。
--   SafeDialer 只对这里登记的 url 的 host:port 精确放行私网 / CGNAT（ADR-0111 决策六第 2 条）。
--   出站令牌（对端签发给本节点的 pmt_…）不存这里，而是写进对应 MCP Server 的 Authorization 头，
--   与其他 MCP Server 的 Bearer 凭证同一存放方式。
CREATE TABLE IF NOT EXISTS mesh_peers (
    name            TEXT    PRIMARY KEY,           -- 本节点给对端起的名字，也是 MCP Server 名（mcp:<name>/<agent>）
    node_id         TEXT    NOT NULL UNIQUE,
    url             TEXT    NOT NULL,              -- 对端源，如 http://100.101.102.103:28888
    public_key      TEXT    NOT NULL,              -- base64url Ed25519 公钥，配对时人工核对指纹后固定
    issued_token_id TEXT    NOT NULL DEFAULT '',   -- 本节点签发给该对端的令牌（mesh_peer_tokens.token_id）
    created_at      INTEGER NOT NULL,              -- Unix 毫秒
    last_seen_at    INTEGER NOT NULL DEFAULT 0,    -- 最近一次探活成功
    last_error      TEXT    NOT NULL DEFAULT ''
) STRICT;
