package instance

import (
	"testing"
)

func TestFormatProviderID(t *testing.T) {
	got := FormatProviderID(12345)
	want := "hcloud://12345"
	if got != want {
		t.Fatalf("FormatProviderID(12345) = %q, want %q", got, want)
	}
}

func TestParseProviderID_Success(t *testing.T) {
	id, raw, err := ParseProviderID("hcloud://987")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 987 {
		t.Fatalf("id = %d, want 987", id)
	}
	if raw != "987" {
		t.Fatalf("raw = %q, want %q", raw, "987")
	}
}

func TestParseProviderID_WrongPrefix(t *testing.T) {
	if _, _, err := ParseProviderID("aws:///42"); err == nil {
		t.Fatal("expected error for wrong prefix, got nil")
	}
}

func TestParseProviderID_BadNumber(t *testing.T) {
	if _, _, err := ParseProviderID("hcloud://notanumber"); err == nil {
		t.Fatal("expected error for non-integer server id, got nil")
	}
}

func TestRoundTrip(t *testing.T) {
	original := int64(42)
	got, _, err := ParseProviderID(FormatProviderID(original))
	if err != nil {
		t.Fatalf("round-trip parse failed: %v", err)
	}
	if got != original {
		t.Fatalf("round-trip id = %d, want %d", got, original)
	}
}
