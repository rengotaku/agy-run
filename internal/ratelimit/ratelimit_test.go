package ratelimit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
