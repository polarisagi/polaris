-- 后台周期任务的"上次跑完"状态（State-in-DB，HE-6）。
-- 空闲自进化调度器据此判断"该任务近期已跑过，无需重启"，重启进程后状态不丢。
-- job_name 为任务标识（如 idle_forgetting / idle_graph_prune）；
-- last_status: success | no_work（探测到无待处理数据，同样计为一次已评估）。
CREATE TABLE IF NOT EXISTS background_job_state (
    job_name    TEXT PRIMARY KEY,
    last_run_at INTEGER NOT NULL, -- Unix 毫秒
    last_status TEXT    NOT NULL CHECK(last_status IN ('success', 'no_work')),
    updated_at  INTEGER NOT NULL  -- Unix 毫秒
) STRICT;
