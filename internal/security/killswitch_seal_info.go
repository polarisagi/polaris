package security

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// 封存现场的读取与人话描述（ADR-0009 决策二配套，2026-08-11 新增）。
//
// 存在理由：`.fullstop` 里本就存着 timestamp / reason / actor 三项现场信息，
// 而启动被拒时的报错原本只有一句 "system is sealed (.fullstop exists in ...);
// remove the file to restart"。用户拿到的是：不知道何时封的、不知道为什么封的、
// 不知道删掉安不安全，也不知道有 `polaris unseal` 这个命令。
//
// 真实案例（2026-08-11）：一次 8 天前的性能漂移写下封存，用户重装后全新安装
// 直接起不来，守护进程每 10s 重启一次撞同一堵墙，日志里只有那一句话。

// SealInfo 是 .fullstop 的内容。
type SealInfo struct {
	Timestamp int64  `json:"timestamp"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor"`
}

// SealFilePath 返回封存标记文件路径。
func SealFilePath(dataDir string) string { return filepath.Join(dataDir, ".fullstop") }

// ReadSeal 读取封存现场。文件不存在时返回 (nil, nil)——"没被封存"不是错误。
//
// 文件存在但内容损坏时返回**非 nil 的 SealInfo**（各字段为零值）与 nil error：
// 封存这一事实由文件的**存在**承载，而非内容的可解析性。内容读不出来不能反过来
// 让调用方以为系统没被封存——那正好把 fail-closed 变成 fail-open。
func ReadSeal(dataDir string) (*SealInfo, error) {
	raw, err := os.ReadFile(SealFilePath(dataDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "读取 .fullstop 失败", err)
	}
	var info SealInfo
	if jsonErr := json.Unmarshal(raw, &info); jsonErr != nil {
		// 刻意吞掉解析错误并返回非 nil SealInfo（nolint:nilerr）：封存这一事实由
		// 文件的**存在**承载，而非内容的可解析性。若因内容损坏而向上报错，调用方
		// 很可能据此认为"读不到封存信息 == 没被封存"，把 fail-closed 变成 fail-open。
		// 原始内容原样带进 Reason，供人工判读。
		return &SealInfo{Reason: "(封存文件内容无法解析：" + string(raw) + ")"}, nil //nolint:nilerr
	}
	return &info, nil
}

// DescribeSeal 生成给人看的封存说明，含现场信息与两条可执行的恢复路径。
func DescribeSeal(dataDir string) string {
	info, err := ReadSeal(dataDir)
	if err != nil || info == nil {
		return "系统处于封存态（.fullstop 存在于 " + dataDir + "）。" +
			"执行 `polaris unseal` 解封后重启。"
	}

	when := "未知时间"
	ago := ""
	if info.Timestamp > 0 {
		t := time.Unix(info.Timestamp, 0)
		when = t.Format("2006-01-02 15:04:05")
		ago = fmt.Sprintf("（%s前）", humanizeDuration(time.Since(t)))
	}
	actor := info.Actor
	if actor == "" {
		actor = "未记录"
	}
	reason := info.Reason
	if reason == "" {
		reason = "未记录"
	}

	return fmt.Sprintf(
		"系统处于封存态（KillSwitch FullStop），拒绝启动。\n"+
			"  封存时间: %s%s\n"+
			"  触发者:   %s\n"+
			"  原因:     %s\n"+
			"  标记文件: %s\n"+
			"\n"+
			"请先按原因排查（审计日志在 %s）。确认无误后解封：\n"+
			"  polaris unseal --reason \"<说明为什么可以恢复>\"\n"+
			"\n"+
			"服务运行中时也可调用 POST /_admin/unseal（需有效 POLARIS_API_KEY）；"+
			"但进程因封存起不来时该端点不可达，此时用上面的命令。",
		when, ago, actor, reason, SealFilePath(dataDir), filepath.Join(dataDir, "audit"))
}

// humanizeDuration 把时长写成中文近似值——精确到秒对"多久以前"这个问题没有意义。
func humanizeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
}

// ClearSeal 删除封存标记。供 `polaris unseal` 在进程未运行时使用。
//
// 与 KillSwitch.ManualRecover 的分工：后者是**进程内活恢复**（ADR-0009 决策二），
// 会同时复位内存态计数器并跑 recoveryCallback；本函数只处理磁盘凭证，用于
// 「进程因封存根本起不来」这一 ManualRecover 够不着的场景。两者都要求人工显式
// 触发，都不构成 ADR-0009 明令禁止的"自动 unseal"。
func ClearSeal(dataDir string) error {
	if err := os.Remove(SealFilePath(dataDir)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return apperr.Wrap(apperr.CodeInternal, "删除 .fullstop 失败", err)
	}
	return nil
}
