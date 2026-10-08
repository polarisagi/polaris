package audioassets

import (
	"strings"
	"testing"
)

// 清单自身格式：任何一项 sha256 / size 录入错误都会让下载永远校验失败。
func TestAll_ManifestWellFormed(t *testing.T) {
	all := All()
	if len(all) != 8 {
		t.Fatalf("清单应含 5 个平台库 + 3 个模型（STT/标点/Melo），got %d", len(all))
	}
	seen := map[string]bool{}
	for _, a := range all {
		if err := a.Validate(); err != nil {
			t.Errorf("%v", err)
		}
		if seen[a.File] {
			t.Errorf("归档名重复：%s", a.File)
		}
		seen[a.File] = true
		if !strings.HasPrefix(a.URL(), "https://github.com/k2-fsa/sherpa-onnx/releases/download/") {
			t.Errorf("URL 前缀异常：%s", a.URL())
		}
	}
}

// 平台库资产名必须带钉死的 ABI 版本：升级 SherpaABIVersion 时库归档名自动跟随，
// 但 sha256 不会——此断言保证两者不会悄悄脱节（版本变了而 sha 仍是旧版的）。
func TestLibAssets_BoundToABIVersion(t *testing.T) {
	for _, p := range LibPlatforms() {
		goos, goarch, _ := strings.Cut(p, "/")
		a, ok := LibAsset(goos, goarch)
		if !ok {
			t.Fatalf("%s 缺少库资产", p)
		}
		if !strings.Contains(a.File, "v"+SherpaABIVersion+"-") || a.Tag != "v"+SherpaABIVersion {
			t.Errorf("%s: 资产 %s / tag %s 未绑定 ABI 版本 %s", p, a.File, a.Tag, SherpaABIVersion)
		}
	}
	if _, ok := LibAsset("plan9", "mips"); ok {
		t.Error("不受支持的平台不得返回资产")
	}
}

// 规格实测值抽查（ADR-0110）：Melo 只能是 fp32 归档；清单不得再含 Kokoro / Matcha
// （Matcha 因训练数据来源不可核验被删除，见 ADR-0110 2026-10-08 修订）。
func TestTTSModels_Pinned(t *testing.T) {
	if m := MeloModel(); m.Size != 167006755 || m.File != "vits-melo-tts-zh_en.tar.bz2" || strings.Contains(m.File, "int8") {
		t.Errorf("Melo 清单项与实测不符：%+v", m)
	}
	for _, a := range All() {
		f := strings.ToLower(a.File)
		if strings.Contains(f, "kokoro") || strings.Contains(f, "matcha") || strings.Contains(f, "vocos") {
			t.Errorf("清单不得再含 Kokoro/Matcha/声码器：%s", a.File)
		}
	}
}

func TestValidate_RejectsBadSHA(t *testing.T) {
	bad := STTModel()
	bad.SHA256 = strings.ToUpper(bad.SHA256)
	if bad.Validate() == nil {
		t.Error("大写 sha256 应被拒绝（下载端按小写比较）")
	}
	bad.SHA256 = "abc"
	if bad.Validate() == nil {
		t.Error("短 sha256 应被拒绝")
	}
}
