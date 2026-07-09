package guard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPIIDetector(t *testing.T) {
	d := NewPIIDetector()
	ctx := context.Background()

	text := "My email is test@example.com and my phone is 13800138000. My AWS key is AKIAIOSFODNN7EXAMPLE."

	if !d.HasPII(text) {
		t.Fatalf("expected HasPII to be true")
	}

	matches, err := d.Detect(ctx, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 3 {
		t.Fatalf("expected 3 matches, got %d", len(matches))
	}

	redacted, count, err := d.Redact(ctx, text)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 redacted items")
	}

	if redacted != "My email is [REDACTED:email] and my phone is[REDACTED:phone_cn]. My AWS key is [REDACTED:aws_key]." {
		t.Fatalf("unexpected redacted text: %s", redacted)
	}

	// Test Presidio
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"start": 0, "end": 2, "entity_type": "PERSON", "score": 0.8}]`))
	}))
	defer ts.Close()

	dP := NewPIIDetectorWithPresidio(ts.URL, ts.Client())
	matchesP, err := dP.Detect(ctx, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(matchesP) != 4 {
		t.Fatalf("expected 4 matches with Presidio, got %d", len(matchesP))
	}
}
