package guard

import (
	"context"
	"strings"
	"testing"
)

// GR-2.1-003：文本中 PII 出现顺序与规则声明顺序不一致时，脱敏结果不得错位/损坏。
func TestRedact_OrderIndependent(t *testing.T) {
	d := NewPIIDetector()
	text := "Tel: 13800138000, Mail: test@example.com, IP 10.1.2.3 end"
	out, n, err := d.Redact(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	want := "Tel: [REDACTED:phone_cn], Mail: [REDACTED:email], IP [REDACTED:ipv4] end"
	if out != want || n != 3 {
		t.Fatalf("got %q (n=%d)\nwant %q", out, n, want)
	}
}

// 重叠命中（身份证号内含手机号样式片段等）必须整段覆盖，不得残留原文。
func TestRedact_OverlappingMatches(t *testing.T) {
	d := NewPIIDetector()
	id := "110101199003071234"
	out, _, err := d.Redact(context.Background(), "id="+id+" ok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "1990") || !strings.HasSuffix(out, " ok") || !strings.HasPrefix(out, "id=") {
		t.Fatalf("overlap redaction corrupted or leaked: %q", out)
	}
}
