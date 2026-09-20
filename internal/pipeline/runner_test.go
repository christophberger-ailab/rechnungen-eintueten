package pipeline

import (
	"testing"
	"time"
)

func TestUntil(t *testing.T) {
	now := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)
	if got := until(now, "11:30"); got != 90*time.Minute {
		t.Errorf("until 11:30 = %v, want 1h30m", got)
	}
	// A time that already passed today happens tomorrow.
	if got := until(now, "09:00"); got != 23*time.Hour {
		t.Errorf("until 09:00 = %v, want 23h", got)
	}
	// Unparseable configuration falls back to 03:00.
	if got := until(now, "kaputt"); got != 17*time.Hour {
		t.Errorf("until garbage = %v, want 17h (fallback 03:00)", got)
	}
}
