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
	base := Fingerprint(prof(16*gib, 8))
	if jitter := Fingerprint(prof(16*gib-200<<20, 8)); jitter != base {
		t.Errorf("200MB 抖动不应改变指纹: %s vs %s", jitter, base)
	}
	if Fingerprint(prof(8*gib, 8)) == base {
		t.Error("内存档位变化应改变指纹")
	}
	if Fingerprint(prof(16*gib, 4)) == base {
		t.Error("核数变化应改变指纹")
	}
	other := prof(16*gib, 8)
	other.GOARCH = "arm64"
	if Fingerprint(other) == base {
		t.Error("arch 变化应改变指纹")
	}
}

func TestBench_PersistRoundTripAndFingerprintInvalidation(t *testing.T) {
	ctx := context.Background()
	store := &memPrefs{}
	fp := Fingerprint(prof(16*gib, 8))
	if _, ok, err := GetBench(ctx, store, fp); ok || err != nil {
		t.Fatalf("空库应返回 ok=false 且无错误，got ok=%v err=%v", ok, err)
	}
	want := BenchRecord{Fingerprint: fp, RTF: 0.42, Supported: true, MeasuredAt: time.Now().UTC()}
	if err := SaveBench(ctx, store, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetBench(ctx, store, fp)
	if !ok || err != nil || got.RTF != 0.42 || !got.Supported {
		t.Fatalf("round trip 失败: %+v ok=%v err=%v", got, ok, err)
	}
	// 指纹变化：旧结论作废（重测）。
	if _, ok, _ := GetBench(ctx, store, Fingerprint(prof(4*gib, 4))); ok {
		t.Error("指纹变化后旧基准必须作废")
	}
}

func TestBench_CorruptRecordIsReportedNotSilent(t *testing.T) {
	store := &memPrefs{m: map[string]string{BenchPrefKey: "{not json"}}
	_, ok, err := GetBench(context.Background(), store, "fp")
	if ok || err == nil {
		t.Errorf("损坏记录应返回错误（调用方据此留痕后重测），got ok=%v err=%v", ok, err)
	}
	store = &memPrefs{getErr: errors.New("db down")}
	if _, _, err := GetBench(context.Background(), store, "fp"); err == nil {
		t.Error("读库失败必须上报")
	}
	store = &memPrefs{putErr: errors.New("readonly")}
	if err := SaveBench(context.Background(), store, BenchRecord{}); err == nil {
		t.Error("写库失败必须上报")
	}
}

// nil store（未接线）安全：不 panic、视为需要重测。
func TestBench_NilStore(t *testing.T) {
	if _, ok, err := GetBench(context.Background(), nil, "fp"); ok || err != nil {
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
	if p.calls != 2 {
		t.Errorf("应预热 1 次 + 测量 1 次，calls=%d", p.calls)
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
