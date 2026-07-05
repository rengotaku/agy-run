package runcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agy.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanSuccessOnly(t *testing.T) {
	log := writeLog(t,
		"conversation=abc",
		"CORTEX_STEP_TYPE_RUN_COMMAND: go test ./...",
		"tests passed",
	)
	if got := Scan(log); len(got) != 0 {
		t.Errorf("no failures expected, got %v", got)
	}
}

func TestScanDetectsFailures(t *testing.T) {
	log := writeLog(t,
		"CORTEX_STEP_TYPE_RUN_COMMAND: go build ./...",
		"error executing cascade step: CORTEX_STEP_TYPE_RUN_COMMAND: /home/x/repo: no such directory",
		"some unrelated line",
		"error executing cascade step: CORTEX_STEP_TYPE_RUN_COMMAND: exit status 1",
	)
	got := Scan(log)
	if len(got) != 2 {
		t.Fatalf("failures = %d, want 2: %v", len(got), got)
	}
	if !strings.Contains(got[0].Detail, "no such directory") {
		t.Errorf("detail = %q", got[0].Detail)
	}
}

func TestScanMissingLog(t *testing.T) {
	if got := Scan(filepath.Join(t.TempDir(), "nope.log")); got != nil {
		t.Errorf("missing log should yield nil, got %v", got)
	}
}

func TestScanIgnoresNonRunCommandErrors(t *testing.T) {
	log := writeLog(t,
		"error executing cascade step: SOMETHING_ELSE: boom",
		"generic error: no such file or directory",
	)
	if got := Scan(log); len(got) != 0 {
		t.Errorf("only RUN_COMMAND errors count, got %v", got)
	}
}

func TestTrimCaps(t *testing.T) {
	long := "CORTEX_STEP_TYPE_RUN_COMMAND: exit status 1 " + strings.Repeat("x", 500)
	log := writeLog(t, long)
	got := Scan(log)
	if len(got) != 1 {
		t.Fatalf("want 1 failure, got %d", len(got))
	}
	if len([]rune(got[0].Detail)) > 302 { // 300 + ellipsis rune tolerance
		t.Errorf("detail not capped: len=%d", len([]rune(got[0].Detail)))
	}
}
