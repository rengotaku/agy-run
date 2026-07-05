package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rengotaku/agy-run/internal/config"
	"github.com/rengotaku/agy-run/internal/emit"
)

// parseResult decodes the single JSON result object agy-run writes to stdout.
func parseResult(t *testing.T, out string) emit.Result {
	t.Helper()
	var r emit.Result
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not a single JSON object: %v\n---\n%s", err, out)
	}
	return r
}

const stubConv = "11111111-1111-1111-1111-111111111111"

// stubAgy is a POSIX-sh stand-in for the real agy binary. It writes a
// conversation line (and optional extra lines) to --log-file, echoes STUB_BODY
// to stdout, and exits STUB_EXIT. Behaviour is driven entirely by env vars so a
// single stub covers every path.
const stubAgy = `#!/bin/sh
if [ "$1" = "models" ]; then
  printf '%s\n' "Gemini 3.5 Flash (High)" "Gemini 3.1 Pro (High)"
  exit 0
fi
logfile=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--log-file" ]; then logfile="$a"; fi
  prev="$a"
done
if [ -n "$logfile" ]; then
  printf 'conversation=%s\n' "${STUB_CONV}" > "$logfile"
  if [ -n "$STUB_LOG_EXTRA" ]; then printf '%s\n' "$STUB_LOG_EXTRA" >> "$logfile"; fi
fi
if [ -n "$STUB_BODY" ]; then printf '%s\n' "$STUB_BODY"; fi
if [ -n "$STUB_SLEEP" ]; then sleep "$STUB_SLEEP"; fi
exit ${STUB_EXIT:-0}
`

func newHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte(stubAgy), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_CONV", stubConv)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	return home
}

func run(t *testing.T, home string, argv []string) (int, string, string) {
	t.Helper()
	cfg, _, err := config.Parse(argv, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out, errb bytes.Buffer
	rc := Run(cfg, Options{Stdout: &out, Stderr: &errb, Home: home, Getenv: os.Getenv})
	return rc, out.String(), errb.String()
}

func TestRunSuccessContract(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello from agy")
	t.Setenv("STUB_EXIT", "0")

	rc, out, errb := run(t, home, []string{"say hi"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	// stdout: exactly one JSON result object carrying body + all metadata.
	r := parseResult(t, out)
	if !strings.Contains(r.Body, "hello from agy") {
		t.Errorf("body = %q", r.Body)
	}
	if r.ConversationID != stubConv {
		t.Errorf("conversation_id = %q, want %s", r.ConversationID, stubConv)
	}
	if r.BodyFile == "" {
		t.Errorf("body_file not set: %+v", r)
	}
	if r.ExitCode != 0 || r.Error != nil {
		t.Errorf("expected clean success, got exit=%d err=%+v", r.ExitCode, r.Error)
	}
	if errb != "" {
		t.Errorf("stderr should be empty on clean success, got %q", errb)
	}

	// body_file materialized on disk with the raw body.
	bodyFile := filepath.Join(home, ".gemini", "antigravity-cli", "last-body", stubConv+".md")
	data, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatalf("body file: %v", err)
	}
	if !strings.Contains(string(data), "hello from agy") {
		t.Errorf("body file content = %q", data)
	}

	// index.jsonl: one record, exit 0, conversation id set.
	idx := filepath.Join(home, "data", "agy-run", "index.jsonl")
	iData, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if !strings.Contains(string(iData), stubConv) || !strings.Contains(string(iData), `"exit_code":0`) {
		t.Errorf("index record = %s", iData)
	}
	if info, _ := os.Stat(idx); info != nil && info.Mode().Perm() != 0o600 {
		t.Errorf("index perm = %o, want 600", info.Mode().Perm())
	}
}

func TestRunUnknownModel(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "should not run")

	rc, out, _ := run(t, home, []string{"-m", "Gemnini 3.5 Flash (Hihg)", "hi"})
	if rc != config.ExitUsage {
		t.Fatalf("rc = %d, want 2 (usage)", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "unknown_model" {
		t.Fatalf("expected unknown_model error, got %+v", r)
	}
	// The valid list is returned so the caller can retry correctly.
	found := false
	for _, m := range r.Error.AvailableModels {
		if m == "Gemini 3.5 Flash (High)" {
			found = true
		}
	}
	if !found {
		t.Errorf("available_models missing valid name: %v", r.Error.AvailableModels)
	}
}

func TestRunValidModelPasses(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "ok")

	rc, out, _ := run(t, home, []string{"-m", "Gemini 3.5 Flash (High)", "hi"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	if r.Error != nil {
		t.Errorf("valid model should not error: %+v", r.Error)
	}
	if !strings.Contains(r.Body, "ok") {
		t.Errorf("body = %q", r.Body)
	}
}

func TestRunRateLimited(t *testing.T) {
	home := newHome(t)
	// Empty body + a rate-limit signature in the log, agy exit 0 -> exit 42.
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_LOG_EXTRA", "HTTP 429 resource_exhausted")
	t.Setenv("STUB_EXIT", "0")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != config.ExitRateLimited {
		t.Fatalf("rc = %d, want 42", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "rate_limited" {
		t.Errorf("result missing rate_limited error: %+v", r)
	}
	if r.ExitCode != config.ExitRateLimited {
		t.Errorf("result exit_code = %d, want 42", r.ExitCode)
	}
}

func TestRunEmptyOutput(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_EXIT", "0")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "empty_output" {
		t.Errorf("result missing empty_output error: %+v", r)
	}
}

// TestRunStallExit44 hangs the stub past the idle limit; the watchdog must kill
// the process group and return exit 44, and stamp the kill file for cooldown.
func TestRunStallExit44(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "thinking...")
	t.Setenv("STUB_SLEEP", "10") // hangs; watchdog should kill well before this
	t.Setenv("AGY_RUN_IDLE_LIMIT", "1")
	t.Setenv("AGY_RUN_POLL", "1")

	rc, out, _ := run(t, home, []string{"long task"})
	if rc != config.ExitStall {
		t.Fatalf("rc = %d, want 44 (stall)", rc)
	}
	// The stall is reported in the single result object: error + exit_code 44.
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "stall" {
		t.Errorf("result missing stall error: %+v", r)
	}
	if r.ExitCode != config.ExitStall {
		t.Errorf("result exit_code = %d, want 44", r.ExitCode)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli", ".agy-run.last-kill")); err != nil {
		t.Errorf("kill stamp not created: %v", err)
	}
}

func TestRunPassesThroughAgyExitCode(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "partial")
	t.Setenv("STUB_EXIT", "7")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != 7 {
		t.Fatalf("rc = %d, want 7", rc)
	}
	r := parseResult(t, out)
	if r.ExitCode != 7 {
		t.Errorf("result exit_code = %d, want 7", r.ExitCode)
	}
	if !strings.Contains(r.Body, "partial") {
		t.Errorf("body = %q", r.Body)
	}
}
