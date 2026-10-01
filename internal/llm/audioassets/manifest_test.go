package audioassets

import (
	"strings"
	"testing"
)

// 清单自身格式：任何一项 sha256 / size 录入错误都会让下载永远校验失败。
func TestAll_ManifestWellFormed(t *testing.T) {
	all := All()
	if len(all) != 8 {
		t.Fatalf("清单应含 5 个平台库 + 3 个模型，got %d", len(all))
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

// 规格实测值抽查：Kokoro 只能是 fp32 单一资产（int8 在无 VNNI x86 上慢于实时）。
func TestKokoroModel_IsFP32Only(t *testing.T) {
	k := KokoroModel()
	if strings.Contains(k.File, "int8") {
		t.Errorf("TTS 只保留 fp32，got %s", k.File)
	}
	if k.Size != 364816464 {
		t.Errorf("Kokoro 归档字节数 %d != 364816464", k.Size)
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
