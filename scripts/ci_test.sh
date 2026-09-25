#!/usr/bin/env bash
# ci_test.sh - 完整的本地 CI 测试验证脚本
# 该脚本复刻了 GitHub Actions (ci.yml) 中的全套流程，遇到错误不会立即中断，
# 而是会执行完所有步骤并在最后汇总报错信息，确保在 Push 之前可以本地提前发现所有问题。
# bash ./scripts/ci_test.sh > ci_test.log 2>&1
#
# 每步完整输出落盘到 $CI_LOG_DIR（默认 .ci-logs/<时间戳>/，.ci-logs/latest 指向最近一次），
# 失败步骤另生成 NN-*.errors.txt 精确摘要。旧版按 "fail|error" 大小写不敏感 grep 再取末 50 行：
# go test -v 下通过用例的 slog ERROR/WARN 行会淹没真正的 --- FAIL，且日志随即删除无从追查。
# 摘要改为按输出格式分节提取：失败用例自身的输出、panic、DATA RACE、编译/lint 问题、Rust 错误块、diff。

# 获取脚本所在目录的上一级（项目根目录）
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

set -o pipefail

CI_LOG_DIR="${CI_LOG_DIR:-$ROOT_DIR/.ci-logs/$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$CI_LOG_DIR"
if [[ "$CI_LOG_DIR" == "$ROOT_DIR/.ci-logs/"* ]]; then
    ln -sfn "$(basename "$CI_LOG_DIR")" "$ROOT_DIR/.ci-logs/latest"
fi

echo "======================================"
echo "    Polaris 本地 CI 完整校验脚本      "
echo "======================================"
echo "日志目录: $CI_LOG_DIR"

# 步骤结果（并行数组，兼容 macOS 自带 bash 3.2，不用关联数组）
STEP_NAMES=()
STEP_STATUS=()
STEP_CODES=()
STEP_SECS=()
STEP_LOGS=()
STEP_CMDS=()
FAILED_IDX=()

# Ctrl-C 只打断当前步骤；后续步骤跳过，但仍输出已完成步骤的汇总与失败摘要
INTERRUPTED=0
trap 'INTERRUPTED=1' INT

# 从步骤日志中按输出格式提取错误。只用 POSIX awk 特性（macOS 为 BWK awk）。
# 分节输出；任何专用节都没命中时才回落到关键词匹配 + 日志末尾。
EXTRACT_AWK='
function add(n, s) { c[n]++; L[n, c[n] % MAXL] = s }
function dump(n, ind,   i, s, r) {
    r = ""
    if (!(n in c)) return r
    s = 1
    if (c[n] > MAXL) { r = ind "…（省略前 " (c[n] - MAXL) " 行，见完整日志）\n"; s = c[n] - MAXL + 1 }
    for (i = s; i <= c[n]; i++) r = r ind L[n, i % MAXL] "\n"
    return r
}
function drop(n,   i) {
    if (!(n in c)) return
    for (i = 0; i < MAXL; i++) delete L[n, i]
    delete c[n]
}
# 头部保留：panic / race 的首行与首个栈帧最关键，超长部分截尾
function addh(n, s) { hc[n]++; if (hc[n] <= MAXH) H[n, hc[n]] = s }
function dumph(n, ind,   i, m, r) {
    r = ""
    if (!(n in hc)) return r
    m = hc[n] < MAXH ? hc[n] : MAXH
    for (i = 1; i <= m; i++) r = r ind H[n, i] "\n"
    if (hc[n] > MAXH) r = r ind "…（省略后 " (hc[n] - MAXH) " 行，见完整日志）\n"
    return r
}
function reset_pkg(   k, n, i, keys) {
    n = 0
    for (k in c) keys[++n] = k
    for (i = 1; i <= n; i++) drop(keys[i])
    delete H; delete hc; delete isfail; delete fl
    nfail = 0; nrace = 0; cur = ""; inpanic = 0; inrace = 0; sep = 0
}
function glob(sec, s) { G[sec] = G[sec] s "\n"; GN[sec]++ }
function on_pkg_fail(line,   pkg, i, n, r, a) {
    split(line, a, "\t"); pkg = a[2]
    npkg++
    r = "▸ 包 " pkg "\n"
    if (line ~ /\[(build|setup) failed\]/) {
        r = r "    编译/装配失败，见「编译 / 静态检查问题」节\n"
    }
    for (i = 1; i <= nfail; i++) {
        n = fl[i]; ntest++
        r = r "  ✗ " n "\n" dump(n, "      ")
    }
    if ("__panic" in hc) r = r "  ⚡ panic / fatal error:\n" dumph("__panic", "      ")
    for (i = 1; i <= nrace && i <= 3; i++) r = r "  ⚠ DATA RACE #" i ":\n" dumph("__race" i, "      ")
    if (nrace > 3) r = r "  ⚠ 另有 " (nrace - 3) " 处 DATA RACE，见完整日志\n"
    # 无 --- FAIL 也无 panic：多为 os.Exit / log.Fatal / TestMain 失败，展示最后运行的用例与包级输出
    if (nfail == 0 && !("__panic" in hc) && nrace == 0 && line !~ /\[(build|setup) failed\]/) {
        if (cur != "" && (cur in c)) r = r "  进程在用例 " cur " 执行期间退出，其输出：\n" dump(cur, "      ")
        if ("" in c) r = r "  包级输出：\n" dump("", "      ")
    }
    T = T r
    reset_pkg()
}
BEGIN { MAXL = 80; MAXH = 60; MAXTAIL = 25; MAXKW = 30; nfail = 0; cur = "" }

# 全局：末尾行与关键词行，仅供兜底
{
    ntail++; TL[ntail % MAXTAIL] = $0
    if ($0 ~ /FAIL|Error|ERROR|error:|fatal|panic:|❌|✗/) { nkw++; KW[nkw % MAXKW] = $0 }
}

# ---- go test -v ----
/^=== (RUN|CONT|NAME|PAUSE) / { cur = $3; gotest = 1; next }
/^[ \t]*--- (PASS|SKIP|FAIL): / {
    t = $0; sub(/^[ \t]+/, "", t); split(t, a, " ")
    if (a[2] == "FAIL:") { if (!(a[3] in isfail)) { isfail[a[3]] = 1; fl[++nfail] = a[3] } }
    else drop(a[3])
    next
}
/^FAIL\t/ { on_pkg_fail($0); next }
/^(ok  |\?   )\t/ { reset_pkg(); next }
/^(PASS|FAIL)$/ || /^coverage: / { next }

/^==================$/ { if (inrace) { inrace = 0 } else { sep = 1 }; next }
sep && /^WARNING: DATA RACE/ { sep = 0; inrace = 1; nrace++; nrace_total++; addh("__race" nrace, $0); next }
{ sep = 0 }
inrace { addh("__race" nrace, $0); next }

/^(panic: |fatal error: )/ { inpanic = 1 }
inpanic { addh("__panic", $0); next }

# ---- Go 编译错误 / go vet / golangci-lint：非缩进的 file.go:行[:列]: 消息 ----
/^# [^ ]/ { lasthash = $0; next }
/^[^ \t:]+\.go:[0-9]+(:[0-9]+)?: / {
    if (lasthash != "") { glob("go", lasthash); lasthash = "" }
    glob("go", $0); ctx = 3; next
}
ctx > 0 && /^([ \t]|\^)/ { ctx--; glob("go", $0); next }
{ ctx = 0 }
/^[0-9]+ issues?:/ || /^\* [a-z0-9]+: [0-9]+/ { glob("go", $0); next }
/level=error/ { glob("go", $0); next }

# ---- Rust / cargo：error 块到空行结束（clippy -D warnings、cargo deny 同格式）----
!gotest && /^error(\[[A-Za-z0-9_-]*\])?: / {
    inerr = 1; nerr++; bl = 0
    if (nerr <= 20) glob("rust", "")
}
inerr && /^[ \t]*$/ { inerr = 0; next }
inerr {
    if (nerr <= 20) { if (bl < 40) glob("rust", $0); else if (bl == 40) glob("rust", "…（块内省略，见完整日志）") }
    bl++; next
}

# ---- cargo fmt --check ----
/^Diff in / { infmt = 1; fl2 = 0 }
infmt && /^[ \t]*$/ { infmt = 0; next }
infmt { if (fl2 < 30) glob("fmt", $0); fl2++; next }

# ---- git diff --exit-code ----
/^diff --git / { indiff = 1 }
indiff && /^(diff --git |index |--- |\+\+\+ |@@|[-+ ])/ {
    if (GN["diff"] < 120) glob("diff", $0); else if (GN["diff"] == 120) glob("diff", "…（diff 过长，见完整日志）")
    next
}
{ indiff = 0 }

# ---- make 失败目标 ----
/^make(\[[0-9]+\])?: \*\*\* / { glob("make", $0); next }

# 其余行归入当前 go 用例（cur 为空即包级）缓冲
{ add(cur, $0) }

END {
    any = 0
    if (T != "") { printf "── 失败的 Go 测试（%d 个包 / %d 个用例）──\n%s\n", npkg, ntest, T; any = 1 }
    if (G["go"] != "") { printf "── 编译 / 静态检查问题 ──\n%s\n", G["go"]; any = 1 }
    if (G["rust"] != "") {
        printf "── Rust / cargo 错误（%d 块%s）──%s\n", nerr, (nerr > 20 ? "，仅列前 20" : ""), G["rust"]; any = 1
    }
    if (G["fmt"] != "") { printf "── 格式化差异 ──\n%s\n", G["fmt"]; any = 1 }
    if (G["diff"] != "") { printf "── 未提交的生成物差异 ──\n%s\n", G["diff"]; any = 1 }
    if (G["make"] != "") { printf "── make 失败目标 ──\n%s\n", G["make"]; any = 1 }
    if (!any) {
        if (nkw > 0) {
            print "── 含错误关键词的行（末 " (nkw < MAXKW ? nkw : MAXKW) " 条）──"
            for (i = (nkw > MAXKW ? nkw - MAXKW + 1 : 1); i <= nkw; i++) print KW[i % MAXKW]
            print ""
        }
        print "── 日志末尾 ──"
        for (i = (ntail > MAXTAIL ? ntail - MAXTAIL + 1 : 1); i <= ntail; i++) print TL[i % MAXTAIL]
    }
}
'

# 去 ANSI 颜色码后交给 awk；ESC 由 bash $'' 展开，BSD sed 不认 \x1b
extract_errors() {
    LC_ALL=C sed $'s/\033\\[[0-9;]*[A-Za-z]//g' "$1" | awk "$EXTRACT_AWK"
}

fmt_secs() {
    local s=$1
    if (( s >= 60 )); then printf '%dm%02ds' $((s / 60)) $((s % 60)); else printf '%ds' "$s"; fi
}

# 封装执行步骤的函数
run_step() {
    local step_name="$1"
    local cmd="$2"
    local idx=${#STEP_NAMES[@]}
    local slug
    slug=$(printf '%02d' $((idx + 1)))
    local log_file="$CI_LOG_DIR/$slug.log"

    STEP_NAMES+=("$step_name")
    STEP_CMDS+=("$cmd")
    STEP_LOGS+=("$log_file")

    if (( INTERRUPTED )); then
        STEP_STATUS+=("SKIP"); STEP_CODES+=("-"); STEP_SECS+=(0)
        return
    fi

    echo ""
    echo -e "\033[1;34m▶ $step_name...\033[0m"
    echo "\$ $cmd" > "$log_file"

    local start=$SECONDS
    # 子 shell 隔离步骤内的 cd / export；PIPESTATUS[0] 取命令自身退出码而非 tee 的
    ( eval "$cmd" ) 2>&1 | tee -a "$log_file"
    local rc=${PIPESTATUS[0]}
    local dur=$((SECONDS - start))

    STEP_CODES+=("$rc")
    STEP_SECS+=("$dur")
    if (( INTERRUPTED )); then
        STEP_STATUS+=("INT")
        FAILED_IDX+=("$idx")
        echo -e "\033[1;33m⏹ 中断: $step_name ($(fmt_secs "$dur"))，后续步骤将跳过\033[0m"
    elif (( rc == 0 )); then
        STEP_STATUS+=("PASS")
        echo -e "\033[1;32m✅ 通过: $step_name ($(fmt_secs "$dur"))\033[0m"
    else
        STEP_STATUS+=("FAIL")
        FAILED_IDX+=("$idx")
        extract_errors "$log_file" > "$CI_LOG_DIR/$slug.errors.txt"
        echo -e "\033[1;31m❌ 失败: $step_name (退出码 $rc, $(fmt_secs "$dur"))\033[0m"
        echo -e "\033[1;31m   完整日志: $log_file\033[0m"
    fi
}

run_step "[1/13] 准备环境: 创建 Mock Web dist" "mkdir -p web/dist && touch web/dist/index.html"

# 确保 golangci-lint 已安装，并加入 PATH 环境变量。
# 版本锁定为 v2.12.2，与 .github/workflows/ci.yml 的 golangci-lint-action
# `version: v2.12.2` 保持单一数据源一致——此前用 @latest 会导致本地
# ci_test.sh 与实际 CI 使用不同版本，规则集变更时本地校验结果与 CI 不一致
# 却难以定位原因（GR-7.3）。
GOLANGCI_LINT_VERSION="v2.12.2"
if ! command -v golangci-lint &> /dev/null; then
    echo "未找到 golangci-lint，正在安装 ${GOLANGCI_LINT_VERSION}..."
    go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"
    export PATH=$PATH:$(go env GOPATH)/bin
fi
run_step "[2/13] 执行跨平台 Go 静态检查 (macOS/Linux/Windows)" "golangci-lint run ./... && GOOS=linux golangci-lint run ./... && GOOS=windows golangci-lint run ./..."

run_step "[3/13] 执行 Rust 格式化与静态检查" "cargo fmt --manifest-path rust/substrate/Cargo.toml --check && cargo clippy --manifest-path rust/substrate/Cargo.toml -- -D warnings"

# 对应 GitHub Actions 的 rust-lint-deny job（make rust-lint && make rust-deny）。
# cargo deny check 扫描 Cargo.lock 中所有依赖的已知漏洞（rustsec advisories）、
# 许可证合规性与来源可信度，是本地最容易漏检的安全门控（GR-7.4）。
# 若 cargo-deny 未安装，自动安装。
if ! command -v cargo-deny &> /dev/null; then
    echo "未找到 cargo-deny，正在安装..."
    cargo install cargo-deny --locked
fi
run_step "[4/13] 执行 Rust 依赖安全审计 (cargo deny)" "make rust-deny"

run_step "[5/13] 执行 docs/arch 一致性检查" "make docs-check && make docs-lint && make docs-refs"

run_step "[6/13] 验证 Spec 一致性 (state.yaml SSoT)" "go test -run \"^TestSpec\" ./internal/protocol/... -v"

run_step "[7/13] 运行 Go 全量单元测试 (带竞争检测与覆盖率)" "make test-ci"

run_step "[8/13] 运行 Rust 单元测试" "make rust-test"

run_step "[9/13] 编译 Rust Substrate 模块" "make rust-build"

run_step "[10/13] 执行全量编译 (make build)" "make build"

run_step "[11/13] 验证多架构交叉编译 (Linux, Windows, macOS)" "GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/polaris && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/polaris && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./cmd/polaris"

run_step "[12/13] 验证生成的配置是否最新" "make gen-threshold-examples && git diff --exit-code configs/threshold-examples/"

run_step "[13/13] 验证 Eval Harness Gate" "POLARIS_DATA_DIR=$(mktemp -d) go run ./cmd/polaris eval --ci-gate"

echo ""
echo "======================================"
echo " 步骤汇总"
echo "======================================"
for i in "${!STEP_NAMES[@]}"; do
    case "${STEP_STATUS[$i]}" in
        PASS) color="1;32"; mark="✅" ;;
        FAIL) color="1;31"; mark="❌" ;;
        INT)  color="1;33"; mark="⏹" ;;
        *)    color="0;37"; mark="⏭" ;;
    esac
    printf "\033[%sm %s %-58s 退出码 %-4s %s\033[0m\n" "$color" "$mark" "${STEP_NAMES[$i]}" \
        "${STEP_CODES[$i]}" "$(fmt_secs "${STEP_SECS[$i]}")"
done

if [ ${#FAILED_IDX[@]} -eq 0 ]; then
    echo "======================================"
    echo -e "\033[1;32m 🎉 所有 CI 测试流程已顺利通过！\033[0m"
    echo -e "\033[1;32m 您现在可以放心地推送到 GitHub。\033[0m"
    echo "======================================"
    exit 0
fi

echo ""
echo -e "\033[1;31m ❌ 本次 CI 校验未通过，失败步骤详情：\033[0m"
for i in "${FAILED_IDX[@]}"; do
    slug=$(printf '%02d' $((i + 1)))
    echo ""
    echo -e "\033[1;31m━━━━ ${STEP_NAMES[$i]}（退出码 ${STEP_CODES[$i]}）━━━━\033[0m"
    echo "  命令:     ${STEP_CMDS[$i]}"
    echo "  完整日志: ${STEP_LOGS[$i]}"
    if [ "${STEP_STATUS[$i]}" = "INT" ]; then
        echo "  （被 Ctrl-C 中断，未提取错误）"
        continue
    fi
    echo "  错误摘要: $CI_LOG_DIR/$slug.errors.txt"
    echo -e "\033[1;33m  ────────────── 错误摘要 ──────────────\033[0m"
    sed 's/^/  /' "$CI_LOG_DIR/$slug.errors.txt"
done
echo ""
echo -e "\033[1;33m 请根据上方日志修复报错信息后再尝试推送。\033[0m"
echo "======================================"
if (( INTERRUPTED )); then exit 130; fi
exit 1
