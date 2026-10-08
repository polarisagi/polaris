package audiorun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/llm/tts"
)

type memPrefs struct {
	m      map[string]string
	getErr error
	putErr error
}

func (p *memPrefs) GetPreference(_ context.Context, k string) (string, error) {
	if p.getErr != nil {
		return "", p.getErr
	}
	return p.m[k], nil
}

func (p *memPrefs) UpsertPreference(_ context.Context, k, v string) error {
	if p.putErr != nil {
		return p.putErr
	}
	if p.m == nil {
		p.m = map[string]string{}
	}
	p.m[k] = v
	return nil
}

// 指纹 = arch + 逻辑核 + 总内存 GiB 取整；VM 统计抖动不应改变指纹，换配置必须改变。
func TestFingerprint(t *testing.T) {
	base := Fingerprint(prof(16*gib, 8), tts.ModelMelo)
	if jitter := Fingerprint(prof(16*gib-200<<20, 8), tts.ModelMelo); jitter != base {
		t.Errorf("200MB 抖动不应改变指纹: %s vs %s", jitter, base)
	}
	if Fingerprint(prof(8*gib, 8), tts.ModelMelo) == base {
		t.Error("内存档位变化应改变指纹")
	}
	if Fingerprint(prof(16*gib, 4), tts.ModelMelo) == base {
		t.Error("核数变化应改变指纹")
	}
	other := prof(16*gib, 8)
	other.GOARCH = "arm64"
	if Fingerprint(other, tts.ModelMelo) == base {
		t.Error("arch 变化应改变指纹")
	}
}

// 指纹必须含模型名：Melo 的基准结论不得被 Matcha 复用，旧 Kokoro 结论也不会被任何新模型读到（ADR-0110 决策 5）。
func TestFingerprint_IncludesModel(t *testing.T) {
	p := prof(16*gib, 8)
	if Fingerprint(p, tts.ModelMelo) == Fingerprint(p, tts.ModelMatcha) {
		t.Error("不同模型的指纹必须不同")
	}
	if !strings.Contains(Fingerprint(p, tts.ModelMatcha), "model=matcha") {
		t.Errorf("指纹应含模型名：%s", Fingerprint(p, tts.ModelMatcha))
	}
	// 各模型独立存档：保存 Melo 的记录不应让 Matcha 读到。
	store := &memPrefs{}
	ctx := context.Background()
	_ = SaveBench(ctx, store, BenchRecord{Fingerprint: Fingerprint(p, tts.ModelMelo), Model: "melo", RTF: 1.2})
	if _, ok, _ := GetBench(ctx, store, tts.ModelMatcha, Fingerprint(p, tts.ModelMatcha)); ok {
		t.Error("Matcha 不得读到 Melo 的基准记录")
	}
	if _, ok, _ := GetBench(ctx, store, tts.ModelMelo, Fingerprint(p, tts.ModelMelo)); !ok {
		t.Error("Melo 记录应可读回")
	}
	// 旧版无后缀键（Kokoro 时代）不再被读取。
	old := &memPrefs{m: map[string]string{BenchPrefKey: `{"fingerprint":"` + Fingerprint(p, tts.ModelMelo) + `","supported":false}`}}
	if _, ok, _ := GetBench(ctx, old, tts.ModelMelo, Fingerprint(p, tts.ModelMelo)); ok {
		t.Error("旧 Kokoro 记录不得复用")
	}
}

func TestBench_PersistRoundTripAndFingerprintInvalidation(t *testing.T) {
	ctx := context.Background()
	store := &memPrefs{}
	fp := Fingerprint(prof(16*gib, 8), tts.ModelMelo)
	if _, ok, err := GetBench(ctx, store, tts.ModelMelo, fp); ok || err != nil {
		t.Fatalf("空库应返回 ok=false 且无错误，got ok=%v err=%v", ok, err)
	}
	want := BenchRecord{Fingerprint: fp, Model: "melo", RTF: 0.42, Supported: true, MeasuredAt: time.Now().UTC()}
	if err := SaveBench(ctx, store, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetBench(ctx, store, tts.ModelMelo, fp)
	if !ok || err != nil || got.RTF != 0.42 || !got.Supported {
		t.Fatalf("round trip 失败: %+v ok=%v err=%v", got, ok, err)
	}
	// 指纹变化：旧结论作废（重测）。
	if _, ok, _ := GetBench(ctx, store, tts.ModelMelo, Fingerprint(prof(4*gib, 4), tts.ModelMelo)); ok {
		t.Error("指纹变化后旧基准必须作废")
	}
}

func TestBench_CorruptRecordIsReportedNotSilent(t *testing.T) {
	store := &memPrefs{m: map[string]string{BenchPrefKey + ".melo": "{not json"}}
	_, ok, err := GetBench(context.Background(), store, tts.ModelMelo, "fp")
	if ok || err == nil {
		t.Errorf("损坏记录应返回错误（调用方据此留痕后重测），got ok=%v err=%v", ok, err)
	}
	store = &memPrefs{getErr: errors.New("db down")}
	if _, _, err := GetBench(context.Background(), store, tts.ModelMelo, "fp"); err == nil {
		t.Error("读库失败必须上报")
	}
	store = &memPrefs{putErr: errors.New("readonly")}
	if err := SaveBench(context.Background(), store, BenchRecord{}); err == nil {
		t.Error("写库失败必须上报")
	}
}

// nil store（未接线）安全：不 panic、视为需要重测。
func TestBench_NilStore(t *testing.T) {
	if _, ok, err := GetBench(context.Background(), nil, tts.ModelMelo, "fp"); ok || err != nil {
		t.Error("nil store 应视为无记录")
	}
	if err := SaveBench(context.Background(), nil, BenchRecord{}); err != nil {
		t.Error("nil store 保存应为空操作")
	}
}

type fakeProvider struct {
	calls    int
	sentence []string
	dur      time.Duration
	delay    time.Duration
	err      error
}

func (f *fakeProvider) Generate(_ context.Context, text string) (tts.Audio, error) {
	f.calls++
	f.sentence = append(f.sentence, text)
	if f.err != nil {
		return tts.Audio{}, f.err
	}
	time.Sleep(f.delay)
	return tts.Audio{Duration: f.dur}, nil
}
func (f *fakeProvider) Close() error { return nil }

// RTF = 合成耗时/音频时长；预热一次丢弃，且两次都用固定句。
func TestRunBench_ComputesRTFAfterWarmup(t *testing.T) {
	p := &fakeProvider{dur: 400 * time.Millisecond, delay: 100 * time.Millisecond}
	rtf, err := RunBench(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1+benchTimedRuns {
		t.Errorf("应预热 1 次 + 计时 %d 次，calls=%d", benchTimedRuns, p.calls)
	}
	for _, s := range p.sentence {
		if s != BenchSentence {
			t.Errorf("基准必须用固定句，got %q", s)
		}
	}
	if rtf < 0.2 || rtf > 0.6 {
		t.Errorf("RTF 应≈0.25，got %.3f", rtf)
	}
	if !strings.Contains(BenchSentence, "二零二六年十月二日") {
		t.Error("固定句应含规格要求的日期部分")
	}
}

func TestRunBench_Errors(t *testing.T) {
	if _, err := RunBench(context.Background(), &fakeProvider{err: errors.New("x")}); err == nil {
		t.Error("合成失败应上报")
	}
	if _, err := RunBench(context.Background(), &fakeProvider{dur: 0}); err == nil {
		t.Error("缺音频时长无法算 RTF，必须报错而非除零")
	}
}
