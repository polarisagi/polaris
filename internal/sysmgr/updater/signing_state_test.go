package updater

import (
	"strings"
	"testing"
)

// TestResolveSigningState 穷举四象限。
//
// 这张表是流水线与客户端之间的协议本身，不是实现细节：改动任一格都意味着
// "什么时候该有签名"的语义变了，两侧必须同步。故用穷举表而非抽样。
func TestResolveSigningState(t *testing.T) {
	cases := []struct {
		name       string
		keyCount   int
		hasPrivKey bool
		want       SigningState
		shouldSign bool
		fatal      bool
	}{
		{
			name: "无公钥无私钥_签名未开通", keyCount: 0, hasPrivKey: false,
			want: SigningDisabled, shouldSign: false, fatal: false,
		},
		{
			name: "有私钥无公钥_签但客户端暂不验", keyCount: 0, hasPrivKey: true,
			want: SigningForward, shouldSign: true, fatal: false,
		},
		{
			name: "公钥私钥齐备_正常态", keyCount: 2, hasPrivKey: true,
			want: SigningEnforced, shouldSign: true, fatal: false,
		},
		{
			// 唯一的致命组合。客户端内嵌公钥后即 fail-closed，此时发不带签名的
			// release 会被每一个已升级客户端拒绝安装，而流水线全绿无人察觉。
			name: "有公钥无私钥_必须阻断发布", keyCount: 1, hasPrivKey: false,
			want: SigningBroken, shouldSign: false, fatal: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveSigningState(c.keyCount, c.hasPrivKey)
			if got != c.want {
				t.Fatalf("ResolveSigningState(%d, %t) = %q，期望 %q", c.keyCount, c.hasPrivKey, got, c.want)
			}
			if got.ShouldSign() != c.shouldSign {
				t.Errorf("%q.ShouldSign() = %t，期望 %t", got, got.ShouldSign(), c.shouldSign)
			}
			if got.IsFatal() != c.fatal {
				t.Errorf("%q.IsFatal() = %t，期望 %t", got, got.IsFatal(), c.fatal)
			}
			if strings.TrimSpace(got.Explain(c.keyCount)) == "" {
				t.Errorf("%q 缺少人话说明——流水线日志里只有一个状态码，运维无从处置", got)
			}
		})
	}
}

// TestSigningBrokenExplainIsActionable 守护致命态的错误文案。
//
// 致命态是最不常发生、也最需要"看一眼就知道怎么办"的一条：真触发时通常是在
// 半夜发版、且触发者未必是当初设计签名的人。文案里必须同时有根因与两条处置路径，
// 否则第一反应会是"把 releasekeys 里的公钥删了让 CI 过去"——而那恰恰会把存量
// 客户端锁死在旧版本（它们的二进制里仍内嵌着公钥，仍在 fail-closed）。
func TestSigningBrokenExplainIsActionable(t *testing.T) {
	msg := SigningBroken.Explain(1)
	for _, want := range []string{
		"COSIGN_PRIVATE_KEY", // 根因
		"fail-closed",        // 为什么会炸
		"无法安装",               // 后果
		"(a)", "(b)",         // 两条处置路径
		"锁死", // 对"直接删公钥"这一错误直觉的预先拦截
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("致命态文案缺少关键信息 %q，运维拿到它无法处置。当前文案:\n%s", want, msg)
		}
	}

	// 轮换顺序在文案里必须是"先公钥后 Secret"。反序会落进本状态：Secret 已换新私钥
	// 而 releasekeys/ 还只有旧公钥，自验找不到匹配的已提交公钥 → 发布中止。
	// 这条断言存在的理由是本文案初稿写反过（"先更新 Secret 再提交新公钥"），
	// 而它恰恰是运维在故障现场唯一会读的一段字。
	pubIdx := strings.Index(msg, "先提交新公钥")
	if pubIdx < 0 {
		t.Fatalf("致命态文案未给出轮换顺序，当前文案:\n%s", msg)
	}
	if secIdx := strings.Index(msg, "再把 Secret 换成新私钥"); secIdx < 0 || secIdx < pubIdx {
		t.Errorf("轮换顺序必须是【先提交新公钥 → 再换 Secret】，当前文案顺序有误:\n%s", msg)
	}
}

// TestResolveSigningStateFromTrustStore 验证内嵌信任根路径与纯函数判定一致。
func TestResolveSigningStateFromTrustStore(t *testing.T) {
	embedded := len(TrustStoreFingerprints())

	t.Run("无私钥", func(t *testing.T) {
		st, n, err := ResolveSigningStateFromTrustStore(false)
		if n != embedded {
			t.Fatalf("公钥数 %d ≠ 内嵌信任根 %d", n, embedded)
		}
		want := ResolveSigningState(embedded, false)
		if st != want {
			t.Fatalf("状态 %q ≠ 纯函数判定 %q", st, want)
		}
		// 致命态必须同时返回 error，否则流水线工具会以 0 退出，阻断失效。
		if st.IsFatal() != (err != nil) {
			t.Errorf("致命态与 error 返回不一致: state=%q err=%v", st, err)
		}
	})

	t.Run("有私钥", func(t *testing.T) {
		st, _, err := ResolveSigningStateFromTrustStore(true)
		if err != nil {
			t.Fatalf("持有私钥时不应为致命态: %v", err)
		}
		if !st.ShouldSign() {
			t.Errorf("持有私钥时必然应当签名，实际 state=%q", st)
		}
	})
}
