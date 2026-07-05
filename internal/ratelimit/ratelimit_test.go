package ratelimit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCountHits(t *testing.T) {
	log := `starting request
HTTP 429 Too Many Requests
model overloaded, retrying
all good here
resource_exhausted: quota exceeded
`
	if got := countHits(strings.NewReader(log)); got != 3 {
		t.Errorf("countHits = %d, want 3", got)
	}
}

func TestCountHitsCaseInsensitiveAndVariants(t *testing.T) {
	for _, sig := range []string{"429", "Overloaded", "RATE-LIMIT", "rate limit", "Resource_Exhausted", "QUOTA"} {
		if got := countHits(strings.NewReader(sig)); got != 1 {
			t.Errorf("countHits(%q) = %d, want 1", sig, got)
		}
	}
}

func TestCountHitsNoMatch(t *testing.T) {
	if got := countHits(strings.NewReader("normal successful response\ndone\n")); got != 0 {
		t.Errorf("countHits = %d, want 0", got)
	}
}

func TestCountHitsMissingFile(t *testing.T) {
	if got := CountHits(filepath.Join(t.TempDir(), "nope.log")); got != 0 {
		t.Errorf("missing file = %d, want 0", got)
	}
}

func TestCountHitsFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agy.log")
	if err := os.WriteFile(p, []byte("429 rate-limit\nok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CountHits(p); got != 1 {
		t.Errorf("CountHits = %d, want 1", got)
	}
}

func TestResetWaitParses(t *testing.T) {
	cases := map[string]time.Duration{
		"RESOURCE_EXHAUSTED. Resets in 3h0m10s.": 3*time.Hour + 10*time.Second,
		"quota reached. Resets in 45m 30s":       45*time.Minute + 30*time.Second,
		"Resets in 90s":                          90 * time.Second,
		"resets in 2h":                           2 * time.Hour,
	}
	for line, want := range cases {
		got, ok := resetWait(strings.NewReader(line))
		if !ok || got != want {
			t.Errorf("resetWait(%q) = %v ok=%v, want %v", line, got, ok, want)
		}
	}
}

func TestResetWaitPrefersLast(t *testing.T) {
	log := "Resets in 3h0m0s\nretry\nResets in 0h0m5s\n"
	got, ok := resetWait(strings.NewReader(log))
	if !ok || got != 5*time.Second {
		t.Errorf("resetWait = %v ok=%v, want 5s (last hint wins)", got, ok)
	}
}

func TestResetWaitNoHint(t *testing.T) {
	if _, ok := resetWait(strings.NewReader("429 rate-limit but no reset info\n")); ok {
		t.Error("no reset hint should yield ok=false")
	}
	if _, ok := ResetWait(filepath.Join(t.TempDir(), "nope.log")); ok {
		t.Error("missing file should yield ok=false")
	}
}
