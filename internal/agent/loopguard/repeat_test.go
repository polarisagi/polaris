package loopguard

import "testing"

func TestObserve_SameCallSameResultEscalates(t *testing.T) {
	d := New(3, 5)
	var levels []Level
	for i := 0; i < 5; i++ {
		levels = append(levels, d.Observe("read_file", []byte(`{"path":"a","n":1}`), "same").Level)
	}
	want := []Level{None, None, Warn, Warn, Stop}
	for i := range want {
		if levels[i] != want[i] {
			t.Fatalf("round %d: want %v got %v (all=%v)", i+1, want[i], levels[i], levels)
		}
	}
}

func TestObserve_ArgOrderInsensitive(t *testing.T) {
	d := New(2, 0)
	d.Observe("t", []byte(`{"a":1,"b":2}`), "r")
	if f := d.Observe("t", []byte(` { "b":2, "a":1 } `), "r"); f.Level != Warn {
		t.Fatalf("键序不同的等价参数应视为同调用: %+v", f)
	}
}

func TestObserve_DifferentArgsOrChangingResultNeverTrips(t *testing.T) {
	d := New(3, 5)
	for i := 0; i < 20; i++ {
		// 参数不同的同名调用：合法
		if f := d.Observe("read_file", []byte(`{"path":"f`+string(rune('a'+i))+`"}`), "x"); f.Level != None {
			t.Fatalf("参数不同不应触发: %+v", f)
		}
	}
	for i := 0; i < 20; i++ {
		// 同参数但结果在变（编辑后重读）：合法
		if f := d.Observe("read_file", []byte(`{"path":"z"}`), string(rune('a'+i))); f.Level != None {
			t.Fatalf("结果变化不应触发: %+v", f)
		}
	}
}

func TestObserve_ResultChangeResetsCount(t *testing.T) {
	d := New(3, 5)
	d.Observe("t", nil, "a")
	d.Observe("t", nil, "a")
	d.Observe("t", nil, "b")
	if f := d.Observe("t", nil, "b"); f.Count != 2 || f.Level != None {
		t.Fatalf("结果变化后应重新计数: %+v", f)
	}
}

func TestObserve_PollingToolsExemptAndDisabled(t *testing.T) {
	d := New(3, 5)
	for i := 0; i < 50; i++ {
		if f := d.Observe("job_status", []byte(`{"id":"1"}`), "running"); f.Level != None {
			t.Fatalf("轮询类工具豁免: %+v", f)
		}
	}
	off := New(0, 0)
	for i := 0; i < 10; i++ {
		if f := off.Observe("x", nil, "r"); f.Level != None {
			t.Fatal("warnAt=0 应关闭检测")
		}
	}
	var nilD *Detector
	if f := nilD.Observe("x", nil, "r"); f.Level != None {
		t.Fatal("nil 检测器应安全")
	}
}

func TestObserve_StopZeroMeansWarnOnly(t *testing.T) {
	d := New(2, 0)
	var f Finding
	for i := 0; i < 30; i++ {
		f = d.Observe("t", nil, "r")
	}
	if f.Level != Warn {
		t.Fatalf("stopAt=0 只提醒不收尾: %+v", f)
	}
}
