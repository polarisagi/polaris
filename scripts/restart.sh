#!/usr/bin/env bash
# 重新编译并重启 Polaris（前端 + 后端）
# 用法：
#   ./scripts/restart.sh          # 构建前端 + Go，重启（复用已有 Rust dylib）
#   ./scripts/restart.sh --full   # 同上 + 重新构建 Rust FFI（Rust 代码有变更时使用）


set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

FULL_BUILD=false
for arg in "$@"; do
  if [[ "$arg" == "--full" ]]; then
    FULL_BUILD=true
  fi
done

PORT=28889
# 默认使用项目内独立目录，不再复用 ~/.polarisagi/polaris（那是 launchd 常驻服务
# com.polarisagi.polaris 的生产数据目录）。两个独立进程同时读写同一个 polaris.db
# 会产生跨进程连接争用——SQLite busy_timeout 兜底但仍可能互相顶牛（例如一方在跑
# 插件市场全量同步时，另一方的查询会短暂失败/挂起），本地测试构建应与常驻服务
# 完全隔离。该目录持久化（不清空），避免每次重启都要重新跑一遍插件市场同步；
# 需要彻底重置就手动删掉 $PROJECT_ROOT/.devdata。仍可用 POLARIS_DATA_DIR 显式覆盖。
DATA_DIR="${POLARIS_DATA_DIR:-$PROJECT_ROOT/.devdata/polaris}"
mkdir -p "$DATA_DIR"
LOG_FILE="$DATA_DIR/logs/polaris.log"
LOG_MAX_BYTES=10485760  # 10 MB，超过则截断

# ── 平台检测 ─────────────────────────────────────────────
case "$(uname -s)" in
  Darwin) DYLIB="libsubstrate.dylib" ;;
  Linux)  DYLIB="libsubstrate.so" ;;
  MINGW*|MSYS*|CYGWIN*) DYLIB="substrate.dll" ;;
  *) echo "✗ 不支持的平台：$(uname -s)"; exit 1 ;;
esac
DYLIB_SRC="rust/substrate/target/release/$DYLIB"
DYLIB_DST="bin/lib/$DYLIB"

# ── 0. 日志截断 ───────────────────────────────────────────
if [[ -f "$LOG_FILE" ]]; then
  size=$(wc -c < "$LOG_FILE" 2>/dev/null || echo 0)
  if (( size > LOG_MAX_BYTES )); then
    echo "→ 日志超过 10MB，截断..."
    tail -c 2097152 "$LOG_FILE" > "${LOG_FILE}.tmp" && mv "${LOG_FILE}.tmp" "$LOG_FILE"
  fi
fi

# ── 1. 停止旧进程（不仅杀 :PORT，也确保 ./bin/polaris 进程完全退出）──────────────────
echo "→ 停止旧进程..."
# 获取占用端口的进程以及当前目录下启动的 ./bin/polaris 进程
get_old_pids() {
  local pids=""
  pids+=$(lsof -ti:"$PORT" 2>/dev/null || true)
  pids+=$'\n'
  pids+=$(pgrep -f "\./bin/polaris" 2>/dev/null || true)
  echo "$pids" | grep -v '^$' | sort -u || true
}

OLD_PIDS=$(get_old_pids)
if [[ -n "$OLD_PIDS" ]]; then
  while IFS= read -r pid; do
    [[ -z "$pid" ]] && continue
    kill "$pid" 2>/dev/null || true
  done <<< "$OLD_PIDS"

  # 等待所有旧进程退出（最多 10s，应对 CrashReporter 或 RocksDB 慢关闭），超时逐个 kill -9
  for i in {1..10}; do
    sleep 1
    STILL_ALIVE=$(get_old_pids)
    if [[ -z "$STILL_ALIVE" ]]; then
      echo "  旧进程已全部退出"
      break
    fi
    if [[ $i == 10 ]]; then
      echo "  优雅退出超时，强制终止..."
      while IFS= read -r pid; do
        [[ -z "$pid" ]] && continue
        kill -9 "$pid" 2>/dev/null || true
      done <<< "$STILL_ALIVE"
      sleep 1
    fi
  done
fi

# 确认端口已释放
if lsof -ti:"$PORT" &>/dev/null; then
  echo "✗ 端口 $PORT 仍被占用，无法启动"
  exit 1
fi

# ── 2. Rust FFI（--full 时重建；否则验证 dylib 存在）──────
if $FULL_BUILD; then
  echo "→ 构建 Rust FFI（--full 模式，约 60~120s）..."
  # CFLAGS= LDFLAGS= 防止 Go/shell 链接标志污染 aws-lc-sys 的 C 编译环境
  CFLAGS= LDFLAGS= cargo build --release --manifest-path rust/substrate/Cargo.toml
else
  if [[ ! -f "$DYLIB_SRC" ]]; then
    echo "✗ Rust dylib 不存在：$DYLIB_SRC"
    echo "  首次使用或 Rust 代码有变更，请运行：./scripts/restart.sh --full"
    exit 1
  fi
  echo "→ 复用已有 Rust dylib（如需重建请加 --full）"
fi

# ── 3. 前端 ───────────────────────────────────────────────
echo "→ 构建前端 (web/)..."
cd web
# 仅当 package.json / package-lock.json 比 node_modules 新时才 install
if [[ ! -d node_modules ]] || \
   [[ package.json -nt node_modules/.package-lock.json ]] || \
   [[ package-lock.json -nt node_modules/.package-lock.json ]]; then
  echo "  npm install..."
  npm install --silent --no-fund --no-audit
else
  echo "  node_modules 已是最新，跳过 npm install"
fi
npm run build
cd ..



# ── 4. 复制 dylib 并构建 Go 后端 ─────────────────────────
echo "→ 构建 Go 后端..."
mkdir -p bin/lib
cp "$DYLIB_SRC" "$DYLIB_DST"
CGO_ENABLED=0 go build -o bin/polaris ./cmd/polaris

# ── 5. 启动 ───────────────────────────────────────────────
echo "→ 启动 Polaris (端口 $PORT)..."
# 每次启动强制使用全新的默认配置
DEV_CONFIG="$DATA_DIR/config_dev.toml"
echo "  清理旧配置..."
rm -rf "$DATA_DIR/config"
mkdir -p "$DATA_DIR/config"
cp configs/defaults.toml "$DEV_CONFIG"

# 强制将开发配置的端口替换为脚本指定的测试端口
sed -i.bak -e "s/port = [0-9]*/port = $PORT/" "$DEV_CONFIG"
rm -f "$DEV_CONFIG.bak"

export POLARIS_CONFIG="$DEV_CONFIG"
# 关键：只把 $DATA_DIR 用于脚本自身记账（日志截断/配置落盘）不会让二进制本身
# 用上这个目录——polaris 进程按 POLARIS_DATA_DIR env > cfg.System.DataDir >
# ~/.polarisagi/polaris 的优先级解析数据目录（见 cmd/polaris/boot_substrate.go:548），
# 必须显式 export 这个 env，否则 SQLite 库依旧会落到默认的生产目录，和 launchd
# 常驻服务撞库。
export POLARIS_DATA_DIR="$DATA_DIR"

mkdir -p "$(dirname "$LOG_FILE")"
nohup ./bin/polaris >> "$LOG_FILE" 2>&1 &

# 等待最多 5s 确认端口监听
for i in {1..10}; do
  sleep 0.5
  NEW_PID=$(lsof -ti:"$PORT" 2>/dev/null || true)
  if [[ -n "$NEW_PID" ]]; then
    echo "✓ Polaris 已启动  PID=${NEW_PID}  http://localhost:${PORT}"
    exit 0
  fi
done

echo "✗ 启动失败，最近日志："
tail -30 "$LOG_FILE"
exit 1
