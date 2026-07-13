// polaris eval 子命令组：V8-S2 Meta-Eval Sentinel（meta_holdout 隔离分区）运维工具。
//
//	polaris eval genkey [--out <目录>]                               本地生成 meta_auditor Ed25519 密钥对
//	polaris eval sign --key <私钥文件>                                本地离线签名（不发网络请求）
//	polaris eval meta-holdout add --file <case.json> --signature <sig>
//	polaris eval meta-audit run --signature <sig>
//	polaris eval meta-audit status
//
// 密钥隔离原则（V8-Principle，见 docs/arch/00-Global-Dictionary.md）：meta_auditor
// 私钥只应存在于运维本地磁盘。genkey/sign 两个子命令完全离线执行，不连接
// POLARIS_SERVER_URL；只有 meta-holdout/meta-audit 两组子命令会发起 HTTP 请求，
// 且请求体只携带签名（signature 字符串），从不携带私钥本身——运行 polaris serve
// 的服务器侧只需要配置对应的公钥（POLARIS_EVAL_PUBKEY_META_AUDITOR）。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func runEvalCmd(args []string) error {
	if len(args) == 0 {
		printEvalHelp()
		return nil
	}
	switch args[0] {
	case "genkey":
		return runEvalGenKey(args[1:])
	case "sign":
		return runEvalSign(args[1:])
	case "meta-holdout":
		return runEvalMetaHoldoutCmd(args[1:])
	case "meta-audit":
		return runEvalMetaAuditCmd(args[1:])
	case "help", "-h", "--help":
		printEvalHelp()
		return nil
	}
	return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知子命令: polaris eval %s", strings.Join(args, " ")))
}

func printEvalHelp() {
	fmt.Println("用法: polaris eval <子命令>")
	fmt.Println()
	fmt.Println("  genkey [--out <目录>]                                    本地生成 meta_auditor 密钥对（私钥不上传，仅本机使用）")
	fmt.Println("  sign --key <私钥文件>                                    对当前时间窗口本地签名（输出 base64，供下方命令使用）")
	fmt.Println("  meta-holdout add --file <case.json> --signature <sig>   写入一条 meta_holdout 隔离测试用例")
	fmt.Println("  meta-audit run --signature <sig>                        触发一次 Meta-Eval 审计并持久化结论")
	fmt.Println("  meta-audit status                                       查看最新一次审计结论")
	fmt.Println()
	fmt.Println("签名有效期 ±2 分钟（防重放窗口），genkey 生成的公钥需设置到运行 polaris serve 的环境：")
	fmt.Println("  POLARIS_EVAL_PUBKEY_META_AUDITOR=<genkey 输出的公钥 base64>")
	fmt.Println()
	fmt.Println("默认情况下 AdvanceGate 不要求 meta_audit 通过（M12EvalThresholds.MetaAuditGateEnabled=false），")
	fmt.Println("需运维在配置中显式开启后，Gate2→Gate3+ 的推进才会依赖 meta-audit run 的最新结论。")
}

// ── polaris eval genkey ──────────────────────────────────────────────────────

func runEvalGenKey(args []string) error {
	outDir := "."
	for i, a := range args {
		if (a == "--out" || a == "-o") && i+1 < len(args) {
			outDir = args[i+1]
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "eval genkey: 生成密钥失败", err)
	}
	keyPath := filepath.Join(outDir, "meta_auditor.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "eval genkey: 写入私钥文件失败", err)
	}
	fmt.Printf("%s  私钥已写入 %s（0600 权限）\n", clr(ansiOk, "✓"), keyPath)
	fmt.Println(clr(ansiWarn, "  请妥善保管此文件：切勿提交到版本库，切勿复制到运行 polaris serve 的服务器。"))
	fmt.Println()
	fmt.Println("公钥（设置到运行 polaris serve 的环境变量 POLARIS_EVAL_PUBKEY_META_AUDITOR）：")
	fmt.Printf("  %s\n", base64.StdEncoding.EncodeToString(pub))
	return nil
}

// ── polaris eval sign ─────────────────────────────────────────────────────────

// runEvalSign 纯本地计算，不发起任何网络请求。签名消息格式必须与
// internal/eval/harness/store.go:verifyEvalSignature 完全一致：
// "{role}:{partition}:{UTC分钟时间戳}"。meta_holdout 只有 RoleMetaAuditor 一个
// 合法身份，因此角色/分区在此硬编码，不对外暴露为参数。
func runEvalSign(args []string) error {
	var keyPath string
	for i, a := range args {
		if (a == "--key" || a == "-k") && i+1 < len(args) {
			keyPath = args[i+1]
		}
	}
	if keyPath == "" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval sign --key <私钥文件>")
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "eval sign: 读取私钥文件失败", err)
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return apperr.New(apperr.CodeInvalidInput, "eval sign: 私钥文件格式无效（应为 polaris eval genkey 生成的 base64 编码文件）")
	}
	payload := []byte(control.RoleMetaAuditor + ":" + control.PartitionMetaHoldout + ":" + time.Now().UTC().Format("200601021504"))
	sig := ed25519.Sign(ed25519.PrivateKey(priv), payload)
	fmt.Println(base64.StdEncoding.EncodeToString(sig))
	fmt.Fprintln(os.Stderr, clr(ansiDim, "（此签名 ±2 分钟内有效，请尽快用于下一条 meta-holdout/meta-audit 命令）"))
	return nil
}

// ── polaris eval meta-holdout ─────────────────────────────────────────────────

func runEvalMetaHoldoutCmd(args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval meta-holdout add --file <case.json> --signature <base64>")
	}
	args = args[1:]
	var file, sig string
	for i, a := range args {
		switch {
		case (a == "--file" || a == "-f") && i+1 < len(args):
			file = args[i+1]
		case (a == "--signature" || a == "-s") && i+1 < len(args):
			sig = args[i+1]
		}
	}
	if file == "" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval meta-holdout add --file <case.json> --signature <base64>")
	}
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "meta-holdout add: 读取用例文件失败", err)
	}
	var c harness.EvalCase
	if err := json.Unmarshal(raw, &c); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "meta-holdout add: 解析用例 JSON 失败", err)
	}
	var result map[string]any
	if err := cliPost("/v1/eval/meta-holdout/cases", map[string]any{"case": c, "signature": sig}, &result); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	fmt.Printf("%s  meta_holdout 用例已写入: id=%v\n", clr(ansiOk, "✓"), result["id"])
	return nil
}

// ── polaris eval meta-audit ────────────────────────────────────────────────────

func runEvalMetaAuditCmd(args []string) error {
	if len(args) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval meta-audit run|status")
	}
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	switch args[0] {
	case "run":
		return runEvalMetaAuditRun(args[1:])
	case "status":
		return runEvalMetaAuditStatus()
	}
	return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval meta-audit run|status")
}

func runEvalMetaAuditRun(args []string) error {
	var sig string
	for i, a := range args {
		if (a == "--signature" || a == "-s") && i+1 < len(args) {
			sig = args[i+1]
		}
	}
	var result map[string]any
	if err := cliPost("/v1/eval/meta-audit", map[string]any{"signature": sig}, &result); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	passed, _ := result["passed"].(bool)
	if passed {
		fmt.Printf("%s  meta_audit 通过\n", clr(ansiOk, "✓"))
	} else {
		fmt.Printf("%s  meta_audit 未通过\n", clr(ansiError, "✗"))
	}
	if reasons, ok := result["failure_reasons"].([]any); ok {
		for _, r := range reasons {
			fmt.Printf("  - %v\n", r)
		}
	}
	return nil
}

func runEvalMetaAuditStatus() error {
	var result map[string]any
	if err := cliGet("/v1/eval/meta-audit", &result); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	recorded, _ := result["recorded"].(bool)
	if !recorded {
		fmt.Println(clr(ansiWarn, "尚未运行过 meta_audit（从未产生审计结论，polaris eval meta-audit run 补齐）"))
		return nil
	}
	passed, _ := result["passed"].(bool)
	computedAt, _ := result["computed_at"].(string)
	status := clr(ansiOk, "通过")
	if !passed {
		status = clr(ansiError, "未通过")
	}
	fmt.Printf("最新 meta_audit 结论: %s（%s）\n", status, computedAt)
	return nil
}
