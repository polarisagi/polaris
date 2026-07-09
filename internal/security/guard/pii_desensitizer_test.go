package guard

import (
	"strings"
	"testing"
)

func TestPIIDesensitizer_Email(t *testing.T) {
	d := NewPIIDesensitizer()
	fake1 := d.Desensitize("email", "test@real.com")
	if !strings.HasSuffix(fake1, "@example.com") {
		t.Errorf("expected @example.com suffix, got %s", fake1)
	}

	fake2 := d.Desensitize("email", "test@real.com")
	if fake1 != fake2 {
		t.Errorf("expected consistency, %s != %s", fake1, fake2)
	}
}

func TestPIIDesensitizer_PhoneCN(t *testing.T) {
	d := NewPIIDesensitizer()
	orig := "13912345678"
	fake := d.Desensitize("phone_cn", orig)
	if len(fake) != 11 {
		t.Errorf("expected len 11, got %d", len(fake))
	}
	if !strings.HasPrefix(fake, "139") {
		t.Errorf("expected prefix 139, got %s", fake)
	}
}

func TestPIIDesensitizer_IDCard(t *testing.T) {
	d := NewPIIDesensitizer()
	fake := d.Desensitize("id_card_cn", "11010519491231002X")
	if len(fake) != 18 {
		t.Errorf("expected len 18, got %d", len(fake))
	}
	if !strings.HasPrefix(fake, "999999") {
		t.Errorf("expected prefix 999999, got %s", fake)
	}
}
