#!/usr/bin/env bash
# release-signing.sh — 发布签名密钥的开通 / 轮换 / 停用 / 自检（ADR-0095 决策二）。
#
# ## 为什么这个脚本可以、也应该入库
#
# 脚本本身不含任何秘密：它**生成**密钥并推进 GitHub Secrets，私钥从不落进仓库。
# 入库的收益是这几条操作从"文档里的一段命令"变成"可复现、可审查、带前置检查的
# 流程"——密钥操作一年也做不了几次，等到真要做时，人早忘光了细节，而这恰恰是
# 最不能出错的时刻。
#
# ## 关键事实：密钥不需要每次发版轮换
#
# 一把密钥签所有 release。生成一次，之后每个 tag 由流水线自动用同一把私钥签名，
# 无需任何人工介入。只有下列情形才轮换：
#   - 私钥可能泄漏（误提交、CI 日志泄露、离职交接）
#   - 长期卫生轮换（数年一次即可）
#   - 算法迁移
#
# ## 顺序为什么被脚本强制
#
# 轮换有一条必须遵守的顺序：**先提交并推送新公钥，再更新 Secret**。
# 反序会让流水线落进 `broken` 判定（Secret 已是新私钥、releasekeys/ 还只有旧公钥
# → 签名自验找不到匹配的已提交公钥 → 发布中止）。本脚本用 git 状态检查把这条
# 顺序做成硬约束，而不是写在文档里指望人记得。
#
# 用法：
#   scripts/release-signing.sh init      首次开通（releasekeys/ 为空时）
#   scripts/release-signing.sh rotate    轮换到新密钥（保留旧公钥以兼容老客户端）
#   scripts/release-signing.sh status    查看当前状态
#   scripts/release-signing.sh verify    对下载的产物离线验签
#   scripts/release-signing.sh retire    停用签名（含防锁死用户的前置检查）

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEYS_DIR="$REPO_ROOT/internal/sysmgr/updater/releasekeys"
SECRET_NAME="POLARIS_RELEASE_PRIVATE_KEY"

# 工作目录建在仓库外：私钥哪怕只存在几秒，也不该出现在 git 工作区里——
# 一次手滑的 `git add -A` 就够了。mktemp -d 默认 0700。
WORK_DIR=""

# ── 输出 ──────────────────────────────────────────────────────────────────────
c_red()  { printf '\033[31m%s\033[0m\n' "$*"; }
c_grn()  { printf '\033[32m%s\033[0m\n' "$*"; }
c_ylw()  { printf '\033[33m%s\033[0m\n' "$*"; }
c_bold() { printf '\033[1m%s\033[0m\n' "$*"; }
die()    { c_red "✗ $*" >&2; exit 1; }

# ── 私钥清理：无论成功、失败还是中断，都必须擦掉 ─────────────────────────────
# trap 覆盖 EXIT/INT/TERM 三种退出路径。只删 WORK_DIR，绝不递归删用户指定目录。
cleanup() {
  [ -n "$WORK_DIR" ] && [ -d "$WORK_DIR" ] || return 0
  local f
  for f in "$WORK_DIR"/*; do
    [ -f "$f" ] || continue
    secure_rm "$f"
  done
  rmdir "$WORK_DIR" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# secure_rm 尽力做安全删除。macOS 用 rm -P，Linux 用 shred；两者都没有时退回 rm
# 并明说——在 SSD + 日志文件系统上这三者的实际差别本就有限，重点是别留副本。
secure_rm() {
  local f="$1"
  if command -v shred > /dev/null 2>&1; then
    shred -u "$f" 2>/dev/null && return 0
  fi
  if [ "$(uname -s)" = "Darwin" ]; then
    rm -P "$f" 2>/dev/null && return 0
  fi
  rm -f "$f"
}

# ── 前置检查 ──────────────────────────────────────────────────────────────────
preflight() {
  command -v openssl > /dev/null 2>&1 || die "缺少 openssl"
  command -v gh      > /dev/null 2>&1 || die "缺少 gh（GitHub CLI）：brew install gh"
  gh auth status > /dev/null 2>&1     || die "gh 未登录：gh auth login"
  command -v go      > /dev/null 2>&1 || die "缺少 go（用于验签自检）"
  [ -d "$KEYS_DIR" ] || die "找不到 $KEYS_DIR —— 是否在 polaris 仓库内运行？"
}

count_pubkeys() { ls -1 "$KEYS_DIR"/*.pem 2> /dev/null | wc -l | tr -d ' '; }

# ── 生成密钥对 ────────────────────────────────────────────────────────────────
# ECDSA P-256、未加密 PKCS#8。不加口令的理由见 releasekeys/README.md：
# 私钥只存在于 GitHub Secrets，口令也得存进同一个库，防御增益接近零，
# 却换来一整类"CI 读不到口令"的失败模式。
generate_keypair() {
  WORK_DIR="$(mktemp -d)"
  openssl ecparam -genkey -name prime256v1 -noout \
    | openssl pkcs8 -topk8 -nocrypt -out "$WORK_DIR/release.key"
  openssl pkey -in "$WORK_DIR/release.key" -pubout -out "$WORK_DIR/release.pub"
  chmod 600 "$WORK_DIR/release.key"
}

# 上传前确认公私钥确实配对：一旦传错，流水线会签出没人能验的签名，
# 而这个错误要等到发版才暴露。
assert_pair_matches() {
  local pub_in_repo="$1"
  openssl pkey -in "$WORK_DIR/release.key" -pubout 2> /dev/null \
    | diff -q - "$pub_in_repo" > /dev/null \
    || die "公私钥不配对——已中止，未上传任何东西"
}

# 端到端自检：走 tools/release_sign.go（流水线真正会跑的那条路径）签一个样本，
# 再用内嵌公钥验回。不做这步就上传，等于把验证推迟到发版当天。
roundtrip_selftest() {
  local sample="$WORK_DIR/selftest.sha256"
  printf 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  selftest.tar.gz\n' > "$sample"
  ( cd "$REPO_ROOT" \
    && POLARIS_RELEASE_PRIVATE_KEY="$(cat "$WORK_DIR/release.key")" \
       env GOOS= GOARCH= go run tools/release_sign.go "$sample" > /dev/null ) \
    || die "端到端自检失败：签名或对照已提交公钥的自验没通过"
  c_grn "✓ 端到端自检通过（签名 → 对照已提交公钥验回）"
}

upload_secret() {
  gh secret set "$SECRET_NAME" < "$WORK_DIR/release.key" \
    || die "上传 Secret 失败"
  c_grn "✓ 私钥已存入 GitHub Secret：$SECRET_NAME"
}

require_clean_worktree() {
  git -C "$REPO_ROOT" diff --quiet && git -C "$REPO_ROOT" diff --cached --quiet \
    || die "工作区有未提交改动。密钥操作会改动 releasekeys/ 并需要提交，请先清理工作区。"
}

# 轮换的硬约束：新公钥必须**已推送**，才允许更新 Secret。
assert_pubkey_pushed() {
  local pem="$1"
  git -C "$REPO_ROOT" ls-files --error-unmatch "$pem" > /dev/null 2>&1 \
    || die "新公钥尚未提交：$pem"
  local upstream
  upstream="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref '@{upstream}' 2> /dev/null || true)"
  [ -n "$upstream" ] || die "当前分支无 upstream，无法确认公钥已推送"
  # 先 fetch：本地记录的 upstream ref 可能是陈旧的，拿它比对会误判成"已推送"。
  git -C "$REPO_ROOT" fetch --quiet 2> /dev/null || true
  git -C "$REPO_ROOT" diff --quiet "$upstream" -- "$pem" \
    || die "新公钥已提交但未推送到 ${upstream}。先 git push，再重跑本命令——
顺序颠倒（先换 Secret 后推公钥）会让流水线判定 broken 并中止发布。"
}

# ── init ──────────────────────────────────────────────────────────────────────
cmd_init() {
  preflight
  local n; n="$(count_pubkeys)"
  [ "$n" -eq 0 ] || die "releasekeys/ 已有 $n 个公钥——开通过了。要换密钥请用 rotate。"
  require_clean_worktree

  local year; year="$(date +%Y)"
  local pem="$KEYS_DIR/release-$year.pem"

  c_bold "▶ 生成 ECDSA P-256 密钥对"
  generate_keypair
  cp "$WORK_DIR/release.pub" "$pem"
  assert_pair_matches "$pem"
  c_grn "✓ 公钥已写入 $(basename "$pem")"

  c_bold "▶ 端到端自检"
  roundtrip_selftest

  c_bold "▶ 上传私钥到 GitHub Secrets"
  upload_secret

  c_bold "▶ 完成"
  cat <<EOF

公钥已就位、私钥已上传、本地私钥将在脚本退出时被安全删除。

下一步（本脚本刻意不代做——密钥入库应经你本人 review）：
  git add $pem
  git commit -m "feat(release): 开通发布签名，内嵌 release-$year.pem"
  git push

推送后的下一个 v* tag 起，产物会带 .sha256.sig，客户端转 fail-closed 验签。
EOF
  print_status
}

# ── rotate ────────────────────────────────────────────────────────────────────
# 单次运行内完成，中途暂停等你提交推送新公钥——用 git 状态把"先公钥后 Secret"
# 这条顺序做成硬约束，而不是写在文档里指望人记得。
cmd_rotate() {
  preflight
  local n; n="$(count_pubkeys)"
  [ "$n" -gt 0 ] || die "releasekeys/ 为空——尚未开通。请先用 init。"

  local year; year="$(date +%Y)"
  local pem="$KEYS_DIR/release-$year.pem"

  # 同年内重复轮换会撞文件名。加序号而不是覆盖：覆盖等于悄悄作废一把可能还在
  # 被老客户端使用的公钥。
  if [ -f "$pem" ]; then
    local i=2
    while [ -f "$KEYS_DIR/release-$year-$i.pem" ]; do i=$((i + 1)); done
    pem="$KEYS_DIR/release-$year-$i.pem"
    c_ylw "同年已有公钥，本次新公钥命名为 $(basename "$pem")"
  fi

  require_clean_worktree
  c_bold "▶ 生成新密钥对（旧公钥保留，轮换期新旧并存）"
  generate_keypair
  cp "$WORK_DIR/release.pub" "$pem"
  assert_pair_matches "$pem"
  c_grn "✓ 新公钥已写入 $(basename "$pem")；旧公钥保留，老客户端仍能验证"

  c_bold "▶ 端到端自检"
  roundtrip_selftest

  c_bold "▶ 提交并推送新公钥（Secret 必须在此之后才更新）"
  cat <<EOF

请在**另一个终端**执行，完成后回到这里按回车：

  cd $REPO_ROOT
  git add $pem
  git commit -m "chore(release): 轮换发布签名密钥，新增 release-$year.pem"
  git push

EOF
  read -r -p "已提交并推送？按回车继续，Ctrl-C 中止： " _
  assert_pubkey_pushed "$pem"
  c_grn "✓ 确认新公钥已推送"

  c_bold "▶ 更新 Secret 为新私钥"
  upload_secret

  cat <<EOF

轮换完成。旧公钥 **暂不要删** ——等绝大多数客户端升级到含新公钥的版本之后，
再删除旧的 release-*.pem 并提交。过早删除会让老客户端无法验证新 release。
EOF
  print_status
}

# ── retire ────────────────────────────────────────────────────────────────────
cmd_retire() {
  preflight
  c_red "停用发布签名是危险操作。"
  cat <<'EOF'

已发布出去的客户端二进制里**仍内嵌着公钥、仍处于 fail-closed**：取不到 .sig
就拒绝安装。因此：

  直接删 Secret  → 流水线发出不带签名的包 → 那些客户端全部拒装 → 用户被锁死在旧版本

正确顺序：
  1. 先从 releasekeys/ 移除全部 .pem 并提交、推送
  2. 发一个过渡版本（此版本二进制不再内嵌公钥，会退回 checksum 校验）
  3. 等绝大多数用户升级到过渡版本之后
  4. 才可以删除 GitHub Secret

本脚本不代做第 4 步，避免顺序被跳过。
EOF
  exit 1
}

# ── verify ────────────────────────────────────────────────────────────────────
cmd_verify() {
  [ $# -eq 2 ] || die "用法: scripts/release-signing.sh verify <文件> <签名文件>"
  command -v go > /dev/null 2>&1 || die "缺少 go"
  ( cd "$REPO_ROOT" && go run ./cmd/polaris release-key verify "$1" "$2" )
}

# ── status ────────────────────────────────────────────────────────────────────
print_status() {
  echo
  c_bold "── 当前发布签名状态 ──"
  local n; n="$(count_pubkeys)"
  if [ "$n" -eq 0 ]; then
    c_ylw "公钥：无（签名未开通）"
  else
    c_grn "公钥：$n 个"
    local f
    for f in "$KEYS_DIR"/*.pem; do
      printf '  %s  %s\n' "$(basename "$f")" \
        "$(openssl pkey -pubin -in "$f" -outform DER 2> /dev/null | shasum -a 256 | cut -c1-16)"
    done
  fi
  if gh secret list 2> /dev/null | grep -q "^$SECRET_NAME"; then
    c_grn "私钥 Secret：已配置（${SECRET_NAME}）"
  else
    c_ylw "私钥 Secret：未配置"
  fi
  echo
  echo "提示：密钥无需每次发版轮换。一把密钥签所有 release，流水线自动完成。"
}

cmd_status() { preflight; print_status; }

# ── 入口 ──────────────────────────────────────────────────────────────────────
case "${1:-}" in
  init)   shift; cmd_init "$@" ;;
  rotate) shift; cmd_rotate "$@" ;;
  retire) shift; cmd_retire "$@" ;;
  verify) shift; cmd_verify "$@" ;;
  status) shift; cmd_status "$@" ;;
  *)
    cat <<'EOF'
release-signing.sh — 发布签名密钥管理（ADR-0095 决策二）

  init                首次开通：生成密钥对、写公钥、自检、上传私钥到 Secrets
  rotate              轮换密钥：新旧公钥并存，强制「先推公钥后换 Secret」的顺序
  status              查看当前公钥与 Secret 配置状态
  verify <文件> <签名>  用内嵌公钥离线验签（等价于 polaris release-key verify）
  retire              停用签名（会先讲清为什么不能直接删 Secret）

密钥**不需要每次发版轮换**——一把密钥签所有 release，流水线自动完成。
只有私钥可能泄漏、或数年一次的卫生轮换时才需要 rotate。

详见 internal/sysmgr/updater/releasekeys/README.md
EOF
    exit 1
    ;;
esac
