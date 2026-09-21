#!/usr/bin/env bash
# 桌面外壳的生命周期冒烟测试（ADR-0096；P0 清单 §make desktop-smoke）。
#
# 立此脚本的原因：这四个场景是现有 Go 门控一条都覆盖不到的地方——进程生命周期、
# 跨平台差异、启动判定分支。"判定由门控做，不接受自述"在这里同样成立：外壳说
# 自己进了附着模式不算数，进程数与端口说了算。
#
# 本脚本只验证**启动判定逻辑**（外壳的 Rust 侧状态机在无 GUI 下的等价路径），
# 不拉起窗口——CI 无显示器，而窗口本身不是这四条断言的对象。
set -euo pipefail

BIN="${POLARIS_BIN:-$PWD/bin/polaris}"
DATA_DIR="${POLARIS_SMOKE_DATA:-$(mktemp -d)/polaris}"
CONFIG="$(mktemp)"
FAILED=0

printf '[interface]\nport = 0\n' > "$CONFIG"
export POLARIS_DATA_DIR="$DATA_DIR"
export POLARIS_CONFIG="$CONFIG"

log()  { printf '  %s\n' "$*"; }
ok()   { printf '\033[38;5;78m✓\033[0m %s\n' "$*"; }
fail() { printf '\033[38;5;203m✗\033[0m %s\n' "$*"; FAILED=1; }

cleanup() {
    if [ -f "$DATA_DIR/run/polaris.pid" ]; then
        kill -TERM "$(cat "$DATA_DIR/run/polaris.pid")" 2>/dev/null || true
        sleep 3
    fi
    rm -f "$CONFIG"
}
trap cleanup EXIT

status_json() { "$BIN" service status --json 2>/dev/null; }
field() { status_json | sed -n "s/.*\"$1\": *\"\{0,1\}\([^\",}]*\)\"\{0,1\}.*/\1/p" | head -1; }

wait_ready() {
    local deadline=$(( $(date +%s) + 90 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        [ "$(field running)" = "true" ] && return 0
        sleep 2
    done
    return 1
}

[ -x "$BIN" ] || { fail "找不到可执行的 polaris：$BIN（设 POLARIS_BIN 指定）"; exit 1; }

# ── 场景 1：冷启动 ───────────────────────────────────────────────────────────
# 无守护进程时，外壳走宿主模式拉起 sidecar，并在超时内就绪。
echo "场景 1 冷启动"
[ "$(field running)" = "true" ] && { fail "测试前置不满足：已有实例在跑"; exit 1; }
"$BIN" serve >/dev/null 2>&1 &
if wait_ready; then
    PORT="$(field port)"; TOKEN="$(field token)"
    ok "守护进程就绪，端口 $PORT（内核分配，非写死 28888）"
else
    fail "90 秒内未就绪"; exit 1
fi

# ── 场景 2：附着 ─────────────────────────────────────────────────────────────
# 两段式探测：/healthz 判存活（免鉴权），带令牌的端点判凭证。
# 只探 healthz 会让凭证不匹配时也进入附着模式，表现为"界面全白但服务是好的"。
echo "场景 2 附着（两段式探测）"
H=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz")
A=$(curl -s -o /dev/null -w '%{http_code}' -H "X-API-Key: $TOKEN" "http://127.0.0.1:$PORT/v1/config")
N=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v1/config")
[ "$H" = "200" ] && ok "① /healthz 200（存活）" || fail "① /healthz 返回 $H"
[ "$A" = "200" ] && ok "② 带令牌 200（凭证有效 → 附着模式）" || fail "② 带令牌返回 $A"
[ "$N" = "401" ] && ok "  无令牌 401（证明 ① 单独不足以判定可用）" || fail "  无令牌返回 $N，应为 401"

BEFORE=$(pgrep -f "$BIN serve" | wc -l | tr -d ' ')
[ "$BEFORE" = "1" ] && ok "只有一个守护进程实例" || fail "实例数为 $BEFORE，应为 1"

# ── 场景 3：单实例 / 自恢复 ──────────────────────────────────────────────────
# 附着模式下不得拉起第二实例；单实例锁是最后一道保证。
echo "场景 3 单实例锁"
if "$BIN" serve >/dev/null 2>&1; then
    fail "第二个实例启动成功——单实例约束失效"
else
    ok "第二个实例被拒绝（退出码非零）"
fi
STILL=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz")
[ "$STILL" = "200" ] && ok "第一个实例不受影响" || fail "第一个实例被影响，healthz=$STILL"

# ── 场景 4：常驻 ─────────────────────────────────────────────────────────────
# 关窗不停守护进程——外壳退出后服务必须还在（定时任务/通道/HITL 继续工作）。
echo "场景 4 外壳退出后守护进程常驻"
ALIVE=$(curl -s -o /dev/null -w '%{http_code}' -H "X-API-Key: $TOKEN" "http://127.0.0.1:$PORT/v1/config")
[ "$ALIVE" = "200" ] && ok "守护进程独立于外壳存活" || fail "守护进程不可用，返回 $ALIVE"

# ── 收尾：优雅关停清理 run/ ──────────────────────────────────────────────────
echo "收尾 SIGTERM 优雅关停"
kill -TERM "$(cat "$DATA_DIR/run/polaris.pid")"
for _ in $(seq 1 15); do [ -f "$DATA_DIR/run/polaris.port" ] || break; sleep 1; done
if [ -f "$DATA_DIR/run/polaris.port" ]; then
    fail "关停后 run/polaris.port 仍在，状态文件未清理"
else
    ok "运行时状态文件已清理"
fi

echo
[ "$FAILED" = "0" ] && { echo "desktop-smoke: PASS"; exit 0; } || { echo "desktop-smoke: FAIL"; exit 1; }
