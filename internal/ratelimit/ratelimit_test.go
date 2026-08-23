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

func TestE1(t *testing.T) {
	line := "I0823 11:43:26.632945 156 quota_manager.go:45] doRefreshQuota: starting reload (force=true)\n"
	if got := countHits(strings.NewReader(line)); got != 0 {
		t.Errorf("TestE1: countHits = %d, want 0", got)
	}
}

func TestE2(t *testing.T) {
	line := "W0823 11:43:23.208429 66 cache.go:123] cache update failed\n"
	if got := countHits(strings.NewReader(line)); got != 0 {
		t.Errorf("TestE2: countHits = %d, want 0", got)
	}
}

func TestE3(t *testing.T) {
	line := "HTTP 429 Too Many Requests\n"
	if got := countHits(strings.NewReader(line)); got < 1 {
		t.Errorf("TestE3: countHits = %d, want >= 1", got)
	}
}

func TestE4(t *testing.T) {
	line := "error: RESOURCE_EXHAUSTED: quota exceeded\n"
	if got := countHits(strings.NewReader(line)); got < 1 {
		t.Errorf("TestE4: countHits = %d, want >= 1", got)
	}
}

func TestE5(t *testing.T) {
	line := "quota exceeded for user\n"
	if got := countHits(strings.NewReader(line)); got < 1 {
		t.Errorf("TestE5: countHits = %d, want >= 1", got)
	}
}

func TestE6(t *testing.T) {
	line := "quotaProject=my-project-123\n"
	if got := countHits(strings.NewReader(line)); got != 0 {
		t.Errorf("TestE6: countHits = %d, want 0", got)
	}
}

func TestE7(t *testing.T) {
	log := `ERROR: logging before google.Init:
I0823 11:43:23.208000 66 main.go:10] starting agy
W0823 11:43:23.208429 66 cache.go:123] cache update failed
I0823 11:43:26.632945 156 quota_manager.go:45] doRefreshQuota: starting reload (force=true)
I0823 11:43:26.633000 156 quota_manager.go:50] reload complete
I0823 11:43:26.634000 156 config.go:80] quotaProject=my-project-123
`
	if got := countHits(strings.NewReader(log)); got != 0 {
		t.Errorf("TestE7: countHits = %d, want 0", got)
	}
}

func TestE8(t *testing.T) {
	log := `ERROR: logging before google.Init:
W0823 11:43:23.208429 66 cache.go:123] cache update failed
I0823 11:43:26.632945 156 quota_manager.go:45] doRefreshQuota: starting reload (force=true)
HTTP 429 Too Many Requests
`
	if got := countHits(strings.NewReader(log)); got < 1 {
		t.Errorf("TestE8: countHits = %d, want >= 1", got)
	}
}

func TestAdd_Status429AndEmbeddedNumbers(t *testing.T) {
	// 追加テスト: TestAdd_Status429AndEmbeddedNumbers / 記号区切りの429検知と埋め込み数値の誤検知排除検証
	if got := countHits(strings.NewReader("status:429 error\n[429] limit\n")); got != 2 {
		t.Errorf("TestAdd_Status429AndEmbeddedNumbers symbol boundary = %d, want 2", got)
	}
	if got := countHits(strings.NewReader("id=1234295\nvar429abc\n")); got != 0 {
		t.Errorf("TestAdd_Status429AndEmbeddedNumbers embedded number = %d, want 0", got)
	}
}


