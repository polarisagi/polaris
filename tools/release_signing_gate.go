//go:build ignore

// release_signing_gate 供 .github/workflows/release.yml 判定本次发布的签名状态。
//
// 存在理由：签名是流水线与客户端的**双侧协议**，两侧对"现在是否该有签名"的判断
// 必须永远一致。若流水线在 YAML 里用 shell 自己判一套（`[ -n "$POLARIS_RELEASE_PRIVATE_KEY" ]`）、
// 客户端用 Go 判另一套，两套判断迟早漂移，而漂移的表现是"发出去的包客户端装不上"。
// 本工具把判定权交还给 internal/sysmgr/updater：读同一个信任根、跑同一个函数。
//
// 用法：
//
//	# release.yml publish job —— 门控模式，broken 时退出 1 中止发布
//	go run tools/release_signing_gate.go >> "$GITHUB_OUTPUT"
//
//	# make release-signing-status —— 报告模式，只打状态不拦构建
//	go run tools/release_signing_gate.go -report-only
//
// 输出（GITHUB_OUTPUT 格式）：
//
//	state=disabled|forward|enforced|broken
//	should_sign=true|false
//	key_count=N
//
// 门控模式下退出码 1 = SigningBroken（公钥已内嵌但流水线无私钥）——必须中止发布，
// 详见 updater.SigningState 文档注释。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/polarisagi/polaris/internal/sysmgr/updater"
)

func main() {
	// -report-only 供本地 `make release-signing-status` 使用。
	//
	// 必须区分两种运行环境，否则会出现一个很别扭的后果：公钥一提交进
	// releasekeys/，本地跑 check-all 就永远失败——因为私钥只存在于 GitHub
	// Secrets，本地判定必然是 broken。而"本地没有私钥"是完全正常的状态，
	// 开发者不该、也不能持有发布私钥。broken 这个判定只在"本应拿得到私钥"
	// 的 CI 环境里才有意义。
	//
	// 用显式 flag 而不是嗅探 CI / GITHUB_ACTIONS 环境变量：后者是隐式魔法，
	// 且真正需要门控的场景（release.yml）恰恰是最不能因环境变量拼写错误而
	// 静默降级为"只报告"的地方——默认严格、按需放宽，方向不能反。
	reportOnly := flag.Bool("report-only", false, "只报告状态，broken 时也不返回非零（本地用）")
	flag.Parse()

	// 私钥不经本进程之手，只判断 CI 是否拿得到它——避免多一条私钥可能被打印/
	// 落盘的代码路径。签名动作本身仍由 cosign 完成。
	hasPrivateKey := os.Getenv("POLARIS_RELEASE_PRIVATE_KEY") != ""

	state, keyCount, err := updater.ResolveSigningStateFromTrustStore(hasPrivateKey)
	if err != nil && !*reportOnly {
		// ::error:: 让 GitHub 在 Summary 里高亮，而不是埋进日志正文。
		fmt.Fprintf(os.Stderr, "::error title=发布签名状态异常::%s\n", state.Explain(keyCount))
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"发布签名：已内嵌 %d 个公钥；本地无 POLARIS_RELEASE_PRIVATE_KEY（正常——发布私钥只存在于 GitHub Secrets）。\n"+
				"实际签名状态由 release 流水线判定。\n", keyCount)
		fmt.Printf("state=%s\nshould_sign=false\nkey_count=%d\n", state, keyCount)
		return
	}

	if state != updater.SigningEnforced {
		fmt.Fprintf(os.Stderr, "::warning title=发布签名::%s\n", state.Explain(keyCount))
	} else {
		fmt.Fprintf(os.Stderr, "::notice title=发布签名::%s\n", state.Explain(keyCount))
	}

	fmt.Printf("state=%s\n", state)
	fmt.Printf("should_sign=%t\n", state.ShouldSign())
	fmt.Printf("key_count=%d\n", keyCount)
}
