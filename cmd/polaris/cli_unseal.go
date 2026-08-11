package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// `polaris unseal` / `polaris seal-status` —— KillSwitch 封存态的本地恢复路径。
//
// # 为什么需要它（ADR-0009 决策二的盲区）
//
// ADR-0009 决策一：守护进程重启检测到 .fullstop → 直接密封、拒绝启动。
// ADR-0009 决策二：恢复走"进程内活恢复"，即 `POST /_admin/unseal`。
//
// 两条单看都对，合起来有个盲区：**活恢复要求进程还活着**。而封存后进程恰恰
// 起不来——服务管理器每 10s 重启一次，每次都撞在决策一那道墙上，`/_admin/unseal`
// 永远不可达。M11 §4.2 也把这两句并排写着，上一句说"立即错误退出"，下一句说
// "恢复：调用 POST /_admin/unseal"，自相矛盾。
//
// 真实后果（2026-08-11）：一次 8 天前的性能漂移写下封存，用户重装后的全新安装
// 直接 brick，除了手动 `rm .fullstop` 无路可走——而报错里并没有告诉他这一点。
//
// ADR-0009 自带的重新评估触发条件是"进程内活恢复模型若在生产中被证明不可靠
// （如恢复端点本身不可达）"。此处正是该条件命中，且本命令**不是**该 ADR 拒绝过
// 的那两个方案：不是"自动 unseal"（仍需人显式执行并给出理由），也不是"重启进程
// 重放事件日志"（不涉及事件重放）。它是第三条路：本地 CLI 恢复。
//
// # 授权强度
//
// 能执行本命令 = 拥有数据目录的文件系统写权限，本就足以直接 `rm .fullstop`。
// 因此 CLI 不比手动删文件更弱；它的价值在于**强制留痕**（写审计事件、要求填
// reason）并把正确做法从"记得去删某个隐藏文件"变成一条可发现的命令。

func runUnsealCmd(args []string) error {
	fs := flag.NewFlagSet("unseal", flag.ContinueOnError)
	reason := fs.String("reason", "", "解封理由（必填，写入审计留痕）")
	actor := fs.String("actor", "", "操作者标识（默认取当前系统用户）")
	force := fs.Bool("force", false, "未处于封存态时也不报错")
	if err := fs.Parse(args); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "unseal: 参数解析失败", err)
	}

	dataDir := resolveDataDirForCLI()

	info, err := security.ReadSeal(dataDir)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "unseal: 读取封存状态失败", err)
	}
	if info == nil {
		if *force {
			fmt.Println("系统未处于封存态，无需解封。")
			return nil
		}
		return apperr.New(apperr.CodeInvalidInput,
			"系统未处于封存态（"+security.SealFilePath(dataDir)+" 不存在），无需解封。")
	}

	// 强制填 reason：解封是把一个安全熔断人为撤销，审计链里必须留下"谁、为什么"。
	// 不给默认值也不允许空串——默认理由等于没有理由。
	if *reason == "" {
		fmt.Fprint(os.Stderr, security.DescribeSeal(dataDir)+"\n\n")
		return apperr.New(apperr.CodeInvalidInput,
			"unseal: 必须用 --reason 说明为什么可以恢复（该理由会写入审计事件）")
	}

	who := *actor
	if who == "" {
		who = currentOSUser()
	}

	// 先打印封存现场再解封：让操作者在动手前最后看一眼自己正在撤销什么。
	fmt.Print(security.DescribeSeal(dataDir))
	fmt.Println()

	if err := security.ClearSeal(dataDir); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "unseal: 清除封存标记失败", err)
	}

	// 审计留痕落在数据目录内的独立文件：此刻进程未启动，DB 与 AuditTrail 都还
	// 没建起来，写不进正规审计链。用追加式文本文件保证这条记录不会因为"当时
	// 系统起不来"而彻底丢失——解封是最需要被追溯的操作之一。
	if err := appendUnsealAudit(dataDir, who, *reason, info); err != nil {
		// 审计写失败不回滚解封（封存已解除，回滚只会让状态更乱），但必须显式告知。
		fmt.Fprintf(os.Stderr, "警告：解封成功但审计记录写入失败：%v\n", err)
	}

	fmt.Printf("✓ 已解封。操作者=%s 理由=%q\n", who, *reason)
	fmt.Println("  现在可以重新启动 polaris。若封存原因未排除，系统可能再次熔断。")
	return nil
}

// runSealStatusCmd 只读查看封存态，不做任何修改。
func runSealStatusCmd() error {
	dataDir := resolveDataDirForCLI()
	info, err := security.ReadSeal(dataDir)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "seal-status: 读取封存状态失败", err)
	}
	if info == nil {
		fmt.Println("系统未处于封存态。")
		return nil
	}
	fmt.Print(security.DescribeSeal(dataDir))
	return nil
}

// appendUnsealAudit 以追加方式记录一次解封。
func appendUnsealAudit(dataDir, actor, reason string, sealed *security.SealInfo) error {
	path := dataDir + "/audit/unseal.log"
	if err := os.MkdirAll(dataDir+"/audit", 0o700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "创建 audit 目录失败", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "打开 "+path+" 失败", err)
	}
	defer f.Close()
	_, err = fmt.Fprintf(f,
		"{\"unsealed_at\":%d,\"actor\":%q,\"reason\":%q,\"sealed_at\":%d,\"sealed_by\":%q,\"sealed_reason\":%q}\n",
		time.Now().Unix(), actor, reason, sealed.Timestamp, sealed.Actor, sealed.Reason)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "写入 "+path+" 失败", err)
	}
	return nil
}

func currentOSUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" { // Windows
		return u
	}
	return "unknown"
}

// resolveDataDirForCLI 与 boot_substrate.go resolveDataDir 同一优先级：
// POLARIS_DATA_DIR env > ~/.polarisagi/polaris。
//
// 刻意不读 config.toml：本命令要在"系统起不来"时可用，而配置加载本身就是启动
// 链路的一环——依赖它会让恢复命令和被恢复的对象共享同一批失败模式。
func resolveDataDirForCLI() string {
	dir := os.Getenv("POLARIS_DATA_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".polarisagi/polaris")
	}
	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, dir[2:])
	}
	return dir
}
