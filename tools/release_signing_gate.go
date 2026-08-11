//go:build ignore

// release_signing_gate 供 .github/workflows/release.yml 判定本次发布的签名状态。
//
// 存在理由：签名是流水线与客户端的**双侧协议**，两侧对"现在是否该有签名"的判断
// 必须永远一致。若流水线在 YAML 里用 shell 自己判一套（`[ -n "$COSIGN_PRIVATE_KEY" ]`）、
// 客户端用 Go 判另一套，两套判断迟早漂移，而漂移的表现是"发出去的包客户端装不上"。
// 本工具把判定权交还给 internal/sysmgr/updater：读同一个信任根、跑同一个函数。
//
// 用法（在 release.yml 的 publish job 内）：
//
//	go run tools/release_signing_gate.go >> "$GITHUB_OUTPUT"
//
// 输出（GITHUB_OUTPUT 格式）：
//
//	state=disabled|forward|enforced
//	should_sign=true|false
//	key_count=N
//
// 退出码 1 = SigningBroken（公钥已内嵌但流水线无私钥）——必须中止发布，
// 详见 updater.SigningState 文档注释。
package main

import (
	"fmt"
	"os"

	"github.com/polarisagi/polaris/internal/sysmgr/updater"
)

func main() {
	// 私钥不经本进程之手，只判断 CI 是否拿得到它——避免多一条私钥可能被打印/
	// 落盘的代码路径。签名动作本身仍由 cosign 完成。
	hasPrivateKey := os.Getenv("COSIGN_PRIVATE_KEY") != ""

	state, keyCount, err := updater.ResolveSigningStateFromTrustStore(hasPrivateKey)
	if err != nil {
		// ::error:: 让 GitHub 在 Summary 里高亮，而不是埋进日志正文。
		fmt.Fprintf(os.Stderr, "::error title=发布签名状态异常::%s\n", state.Explain(keyCount))
		os.Exit(1)
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
