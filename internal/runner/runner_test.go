package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rengotaku/agy-run/internal/config"
	"github.com/rengotaku/agy-run/internal/emit"
	"github.com/rengotaku/agy-run/internal/index"
	"github.com/rengotaku/agy-run/internal/watchdog"
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
// conversation line (plus the cwd and optional extra lines) to --log-file,
// echoes STUB_BODY to stdout, and exits STUB_EXIT. Behaviour is driven entirely
// by env vars so a single stub covers every path:
//
//	STUB_RL_MODEL  rate-limit (empty body + 429) when --model equals this
//	STUB_RL_UNTIL  rate-limit the first N calls (needs STUB_COUNT_FILE)
//	STUB_COUNT_FILE file used to count invocations across retries
//	STUB_RESET     the "Resets in <dur>" value written on a rate-limit
const stubAgy = `#!/bin/sh
if [ "$1" = "models" ]; then
  printf 'Fetching available models...\n'
  printf 'gemini-3.5-flash-high\tGemini 3.5 Flash (High)\n'
  printf 'gemini-3.1-pro-high\tGemini 3.1 Pro (High)\n'
  exit 0
fi
logfile=""
model=""
prev=""
for a in "$@"; do
  case "$prev" in
    --log-file) logfile="$a" ;;
    --model) model="$a" ;;
  esac
  prev="$a"
done
n=0
if [ -n "$STUB_COUNT_FILE" ]; then
  n=$(cat "$STUB_COUNT_FILE" 2>/dev/null || echo 0)
  echo $((n + 1)) > "$STUB_COUNT_FILE"
fi
rl=0
if [ -n "$STUB_RL_MODEL" ] && [ "$model" = "$STUB_RL_MODEL" ]; then rl=1; fi
if [ -n "$STUB_RL_UNTIL" ] && [ "$n" -lt "$STUB_RL_UNTIL" ]; then rl=1; fi
if [ -n "$logfile" ]; then
  printf 'conversation=%s\n' "${STUB_CONV}" > "$logfile"
  printf 'cwd=%s\n' "$(pwd)" >> "$logfile"
  if [ -n "$STUB_LOG_EXTRA" ]; then printf '%s\n' "$STUB_LOG_EXTRA" >> "$logfile"; fi
  if [ "$rl" = "1" ]; then
    printf 'HTTP 429 resource_exhausted. Resets in %s\n' "${STUB_RESET:-0h0m1s}" >> "$logfile"
  fi
fi
if [ "$rl" = "1" ]; then exit 0; fi
if [ -n "$STUB_BODY" ]; then printf '%s\n' "$STUB_BODY"; fi
if [ -n "$STUB_PID_FILE" ]; then echo $$ > "$STUB_PID_FILE"; fi
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

// run wraps config.Parse + Run for the test suite. Sandbox now defaults to ON
// (issue #15), but most of these tests exercise unrelated behaviour (model
// fallback, watchdog, RUN_COMMAND audit, ...) and must not depend on bwrap /
// D-Bus being available in the test environment. So the helper prepends
// "--no-sandbox" to every call; the handful of sandbox-specific tests below
// override it by passing an explicit "--sandbox" later in argv (the later
// flag occurrence wins — see config.Parse's option loop).
func run(t *testing.T, home string, argv []string) (int, string, string) {
	t.Helper()
	full := append([]string{"--no-sandbox"}, argv...)
	cfg, _, err := config.Parse(full, os.Getenv)
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

	rc, out, _ := run(t, home, []string{"-m", "gemini-3.5-flash-invalid", "hi"})
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
		if m == "gemini-3.5-flash-high" {
			found = true
		}
		if strings.Contains(m, "\t") || strings.Contains(m, "Gemini") || strings.Contains(m, "Fetching") {
			t.Errorf("available_models contain unparsed format or progress line: %q", m)
		}
	}
	if !found {
		t.Errorf("available_models missing valid name: %v", r.Error.AvailableModels)
	}
	if len(r.Error.AvailableModels) != 2 {
		t.Errorf("available_models count = %d, want 2: %v", len(r.Error.AvailableModels), r.Error.AvailableModels)
	}
}

func TestRunValidModelPasses(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "ok")

	rc, out, _ := run(t, home, []string{"-m", "gemini-3.5-flash-high", "hi"})
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

// T8 (issue #22 follow-up): agy's OTHER quota shape — exit 1 (not the
// clean-exit-0 the original rate-limit detection assumed) with an empty body
// and a quota/rate-limit signature in the log (e.g. "Individual quota
// reached ... Resets in 37m2s"). Real-world runs showed this was the
// dominant shape behind the exit-1 spike, and previously left error.reason
// unset entirely (exit code never normalized to 42, so the -m fallback chain
// and --wait-quota never fired on it). Per the coordinator's brief, the
// "next candidate gets tried" chain assertion is intentionally not
// re-verified here — TestRunModelFallback already covers that once rc==42,
// the model retries; this test's job is confirming rc gets normalized to 42
// from this exit-1 shape in the first place.
func TestRunQuotaExit1NormalizesToRateLimited(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_EXIT", "1")
	t.Setenv("STUB_LOG_EXTRA", "Error: Individual quota reached. Resets in 37m2s")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != config.ExitRateLimited {
		t.Fatalf("rc = %d, want 42 (rate_limited normalization)", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "rate_limited" {
		t.Errorf("result missing rate_limited error: %+v", r)
	}
	if r.ExitCode != config.ExitRateLimited {
		t.Errorf("result exit_code = %d, want 42", r.ExitCode)
	}

	idx := filepath.Join(home, "data", "agy-run", "index.jsonl")
	data, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if !strings.Contains(string(data), `"error_class":"rate_limited"`) {
		t.Errorf("index record missing error_class=rate_limited, got %s", data)
	}
}

// T9 (issue #22 follow-up): a non-zero agy exit with no rate-limit signature
// must not be silently left unclassified — it falls back to the generic
// "agy_error" so index.jsonl stays diagnosable, but the exit code itself is
// left untouched (its meaning must not change).
func TestRunOtherFailureClassifiedAsAgyError(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_EXIT", "1")
	t.Setenv("STUB_LOG_EXTRA", "Error: something else went wrong")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (unchanged)", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "agy_error" {
		t.Errorf("result missing agy_error: %+v", r)
	}
	if r.ExitCode != 1 {
		t.Errorf("result exit_code = %d, want 1", r.ExitCode)
	}

	idx := filepath.Join(home, "data", "agy-run", "index.jsonl")
	data, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if !strings.Contains(string(data), `"error_class":"agy_error"`) {
		t.Errorf("index record missing error_class=agy_error, got %s", data)
	}
}

// TestRunRateLimitedRecordsErrorClassInIndex (issue #22): a failing run's
// index.jsonl record must carry the same error_class as the stdout result, so
// index.jsonl stops being an undiagnosable blank for the 31% of runs that
// exit non-zero (the motivating measurement in issue #22).
func TestRunRateLimitedRecordsErrorClassInIndex(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_LOG_EXTRA", "HTTP 429 resource_exhausted")
	t.Setenv("STUB_EXIT", "0")

	rc, _, _ := run(t, home, []string{"do work"})
	if rc != config.ExitRateLimited {
		t.Fatalf("rc = %d, want 42", rc)
	}

	idx := filepath.Join(home, "data", "agy-run", "index.jsonl")
	data, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if !strings.Contains(string(data), `"error_class":"rate_limited"`) {
		t.Errorf("index record missing error_class, got %s", data)
	}
}

// TestRunSuccessOmitsErrorClassInIndex (issue #22): a clean success must keep
// the pre-existing index.jsonl shape — no error_class key at all — so
// existing readers (agy-review) are unaffected.
func TestRunSuccessOmitsErrorClassInIndex(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello from agy")
	t.Setenv("STUB_EXIT", "0")

	rc, _, _ := run(t, home, []string{"say hi"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}

	idx := filepath.Join(home, "data", "agy-run", "index.jsonl")
	data, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if strings.Contains(string(data), "error_class") {
		t.Errorf("success record should omit error_class, got %s", data)
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

// latestLog returns the newest agy-run log file under the state dir.
func latestLog(t *testing.T, home string) string {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(home, ".gemini", "antigravity-cli", "log", "agy-run-*.log"))
	if err != nil || len(logs) == 0 {
		t.Fatalf("no log file found: %v", err)
	}
	newest, newestMod := "", time.Time{}
	for _, l := range logs {
		if info, e := os.Stat(l); e == nil && info.ModTime().After(newestMod) {
			newest, newestMod = l, info.ModTime()
		}
	}
	return newest
}

// TestRunWorkspaceSetsCmdDir (issue #1): -W pins agy's cwd so RUN_COMMAND
// resolves in the right tree. The stub records its $PWD into the log.
func TestRunWorkspaceSetsCmdDir(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "done")
	workDir := t.TempDir()

	rc, _, _ := run(t, home, []string{"-W", workDir, "build it"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	log, _ := os.ReadFile(latestLog(t, home))
	realWork, _ := filepath.EvalSymlinks(workDir)
	if !strings.Contains(string(log), "cwd="+workDir) && !strings.Contains(string(log), "cwd="+realWork) {
		t.Errorf("agy cwd not set to -W dir; log:\n%s", log)
	}
}

// TestRunModelFallback (issue #3): -m "A,B" with A rate-limited must retry on B
// and report the model actually used.
func TestRunModelFallback(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "ok from fallback")
	t.Setenv("STUB_RL_MODEL", "gemini-3.5-flash-high") // A rate-limits

	rc, out, errb := run(t, home,
		[]string{"-m", "gemini-3.5-flash-high,gemini-3.1-pro-high", "do it"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0 (fallback should succeed)\nstderr: %s", rc, errb)
	}
	r := parseResult(t, out)
	if r.Model != "gemini-3.1-pro-high" {
		t.Errorf("used model = %q, want the fallback candidate", r.Model)
	}
	if r.Error != nil {
		t.Errorf("fallback success should have no error: %+v", r.Error)
	}
	if !strings.Contains(errb, "次候補") {
		t.Errorf("expected fallback note on stderr, got %q", errb)
	}
}

// TestRunModelFallbackExhausted (issue #3): every candidate rate-limited keeps
// the exit-42 contract and lists what was tried.
func TestRunModelFallbackExhausted(t *testing.T) {
	home := newHome(t)
	// Rate-limit every attempt (both candidates) via the invocation counter.
	t.Setenv("STUB_RL_UNTIL", "9")
	t.Setenv("STUB_COUNT_FILE", filepath.Join(t.TempDir(), "n"))

	rc, out, _ := run(t, home,
		[]string{"-m", "gemini-3.5-flash-high,gemini-3.1-pro-high", "do it"})
	if rc != config.ExitRateLimited {
		t.Fatalf("rc = %d, want 42", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "rate_limited" {
		t.Fatalf("want rate_limited, got %+v", r.Error)
	}
	if len(r.Error.ModelsTried) != 2 {
		t.Errorf("models_tried = %v, want both candidates", r.Error.ModelsTried)
	}
}

// TestRunWaitQuota (issue #3): --wait-quota parses "Resets in" and retries once
// after (a stubbed) wait; the second call succeeds.
func TestRunWaitQuota(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "ok after wait")
	t.Setenv("STUB_RL_UNTIL", "1") // only the first call rate-limits
	t.Setenv("STUB_RESET", "0h0m2s")
	t.Setenv("STUB_COUNT_FILE", filepath.Join(t.TempDir(), "n"))

	var slept time.Duration
	orig := sleepFor
	sleepFor = func(d time.Duration) { slept = d }
	defer func() { sleepFor = orig }()

	rc, out, errb := run(t, home, []string{"--wait-quota", "do it"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0 (retry after wait)\nstderr: %s", rc, errb)
	}
	if slept != 2*time.Second {
		t.Errorf("slept = %v, want 2s (parsed from Resets in)", slept)
	}
	r := parseResult(t, out)
	if !strings.Contains(r.Body, "ok after wait") {
		t.Errorf("body = %q", r.Body)
	}
}

// TestRunRunCommandFailureWarns (issue #4): a failed RUN_COMMAND step in the log
// surfaces as a warning and a non-zero run_command_failures, even when agy's
// body claims success and exits 0.
func TestRunRunCommandFailureWarns(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "最後に go test を実行しビルドとテストが共に緑です")
	t.Setenv("STUB_LOG_EXTRA",
		"error executing cascade step: CORTEX_STEP_TYPE_RUN_COMMAND: /home/x/repo: no such directory")

	rc, out, errb := run(t, home, []string{"implement"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0 (agy self-reported success)", rc)
	}
	r := parseResult(t, out)
	if r.RunCommandFailures != 1 {
		t.Errorf("run_command_failures = %d, want 1", r.RunCommandFailures)
	}
	if len(r.Warnings) == 0 || !strings.Contains(strings.Join(r.Warnings, "\n"), "RUN_COMMAND") {
		t.Errorf("expected RUN_COMMAND warning, got %v", r.Warnings)
	}
	if !strings.Contains(errb, "warning:") {
		t.Errorf("warning should also reach stderr, got %q", errb)
	}
}

// TestRunWorkspacePreflightWarns (issue #2): running against a feature worktree
// whose sibling main/ is the trusted workspace warns that agy writes to main.
func TestRunWorkspacePreflightWarns(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "edited")

	container := t.TempDir()
	mainWT := filepath.Join(container, "main")
	featWT := filepath.Join(container, "feat")
	for _, d := range []string{mainWT, featWT} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// agy trusts only main/.
	settings := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"trustedWorkspaces":["` + mainWT + `"]}`
	if err := os.WriteFile(settings, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Caller works in the feature worktree via -W.
	rc, out, errb := run(t, home, []string{"-W", featWT, "edit files"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	joined := strings.Join(r.Warnings, "\n")
	if !strings.Contains(joined, "trusted workspace") {
		t.Errorf("expected workspace-mismatch warning, got %v", r.Warnings)
	}
	if !strings.Contains(errb, "warning:") {
		t.Errorf("preflight warning should reach stderr, got %q", errb)
	}
}

// TestRunSandboxDegradesWithoutBwrap (issue #15): sandbox requested (default
// ON, or explicit --sandbox) but bwrap is not on PATH must NOT hard-fail —
// it degrades to a plain (unsandboxed) run, surfaces exactly one warning
// mentioning bwrap, and still returns the agy body/exit code normally.
//
// bwrap availability is environment-dependent (the dev/CI box may or may not
// have it installed), so this test does not rely on the ambient PATH: it
// points PATH at a directory containing only the agy stub (no bwrap), which
// makes exec.LookPath("bwrap") fail deterministically regardless of the host.
func TestRunSandboxDegradesWithoutBwrap(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello despite no bwrap")
	t.Setenv("STUB_EXIT", "0")
	t.Setenv("PATH", filepath.Join(home, ".local", "bin")) // agy stub only, no bwrap

	rc, out, _ := run(t, home, []string{"--sandbox", "hi"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0 (degrade, not a hard failure)", rc)
	}
	r := parseResult(t, out)
	if r.Error != nil {
		t.Fatalf("bwrap-missing degrade must not produce an error, got %+v", r.Error)
	}
	if !strings.Contains(r.Body, "hello despite no bwrap") {
		t.Errorf("body = %q, want the degraded run to still complete", r.Body)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "bwrap") {
		t.Fatalf("expected exactly one bwrap warning, got %v", r.Warnings)
	}
	if r.NetLog != "" {
		t.Errorf("net_log = %q, want empty on degrade", r.NetLog)
	}

	// index.jsonl must record the degrade honestly: sandbox:false, no net_log.
	idx := filepath.Join(home, "data", "agy-run", "index.jsonl")
	data, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if !strings.Contains(string(data), `"sandbox":false`) {
		t.Errorf("index record should show sandbox:false on degrade, got %s", data)
	}
	if strings.Contains(string(data), "net_log") {
		t.Errorf("index record should omit net_log on degrade, got %s", data)
	}
}

// TestRunWithoutSandboxHasNoNetLogField: with sandbox explicitly disabled
// (--no-sandbox, as the run() test helper defaults to), result.net_log stays
// unset — it is only ever populated when an attempt actually ran under bwrap.
func TestRunWithoutSandboxHasNoNetLogField(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello")

	rc, out, _ := run(t, home, []string{"hi"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	if r.NetLog != "" {
		t.Errorf("net_log = %q, want empty when sandbox is disabled", r.NetLog)
	}
	if strings.Contains(out, "net_log") {
		t.Errorf("stdout JSON should omit net_log entirely, got: %s", out)
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

func TestRunLegacyModelFormat(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyStub := `#!/bin/sh
if [ "$1" = "models" ]; then
  printf '%s\n' "Gemini 3.5 Flash (High)" "Gemini 3.1 Pro (High)"
  exit 0
fi
printf 'ok\n'
`
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte(legacyStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_CONV", stubConv)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	rc, out, _ := run(t, home, []string{"-m", "Gemini 3.5 Flash (High)", "hi"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0 (legacy format model should pass)", rc)
	}
	r := parseResult(t, out)
	if r.Error != nil {
		t.Errorf("legacy model format should not error: %+v", r.Error)
	}
}

// 追加テスト: IndexEffectiveModelResolution / -m 未指定時に settings.json から実効モデルが index.jsonl に記録されるかの統合テスト
func TestRunIndexEffectiveModelResolution(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, "data", "agy-run")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
if [ "$1" = "models" ]; then
  printf 'gemini-3.1-pro-high\tGemini 3.1 Pro (High)\n'
  exit 0
fi
printf 'ok\n'
`
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	settingsDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"model":"Gemini 3.1 Pro (High)"}`), 0o644)

	t.Setenv("STUB_CONV", stubConv)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	rc, _, _ := run(t, home, []string{"hello"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}

	indexPath := filepath.Join(stateDir, "index.jsonl")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("failed to read index.jsonl: %v", err)
	}
	if !strings.Contains(string(data), `"model":"gemini-3.1-pro-high"`) {
		t.Errorf("index content = %s, want model to be resolved as gemini-3.1-pro-high", string(data))
	}
}

// 回帰テスト: TestC12_ModelFlagSkipsModelListLookup / -m 指定時に対応表の取得を行わず指定値がそのまま記録されること
func TestC12_ModelFlagSkipsModelListLookup(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\nexit 0\n"
	agyPath := filepath.Join(binDir, "agy")
	if err := os.WriteFile(agyPath, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	paths := DefaultPaths(home, func(string) string { return "" })
	paths.AgyBin = agyPath

	listCalled := false
	orig := listModelMapFunc
	listModelMapFunc = func(agyBin string) ([]string, map[string]string, bool) {
		listCalled = true
		return orig(agyBin)
	}
	t.Cleanup(func() {
		listModelMapFunc = orig
	})

	cfg := config.Config{
		Model: "gemini-3.6-flash-high",
	}

	_, _, _ = attempt(context.Background(), cfg, "gemini-3.6-flash-high", Options{Home: home, Getenv: func(string) string { return "" }}, emit.New(io.Discard, io.Discard), paths)

	if listCalled {
		t.Errorf("expected listModelMapFunc not to be called in attempt when model is specified, but it was called")
	}

	indexPath := paths.IndexFile
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("failed to read index.jsonl: %v", err)
	}
	if !strings.Contains(string(data), `"model":"gemini-3.6-flash-high"`) {
		t.Errorf("index content = %s, want model gemini-3.6-flash-high", string(data))
	}
}





// A7: 本文ゼロ + ログに rate limit キーワード + 成果物ツール呼び出しあり ->
// runner のエラー分類で rate_limited (exit 42) が優先され、artifact_missing に上書きされない
func TestA7_RunRateLimitedTakesPrecedenceOverArtifactMissing(t *testing.T) {
	home := newHome(t)
	// 本文ゼロ + rate limit ログ
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_LOG_EXTRA", "HTTP 429 resource_exhausted")
	t.Setenv("STUB_EXIT", "0")

	// transcript に generate_image の tool_calls がある状態を仮定
	// (brainDir 以下に transcript.jsonl を作成)
	convDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", stubConv, ".system_generated", "logs")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptContent := `{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"generate_image","args":{}}]}` + "\n"
	if err := os.WriteFile(filepath.Join(convDir, "transcript.jsonl"), []byte(transcriptContent), 0o644); err != nil {
		t.Fatal(err)
	}

	rc, out, _ := run(t, home, []string{"generate image prompt"})
	if rc != config.ExitRateLimited {
		t.Fatalf("rc = %d, want 42 (rate_limited must take precedence over artifact_missing)", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "rate_limited" {
		t.Errorf("result missing rate_limited error (got reason %q): %+v",
			r.Error.Reason, r.Error)
	}
	if r.ExitCode != config.ExitRateLimited {
		t.Errorf("result exit_code = %d, want 42", r.ExitCode)
	}
}

// B1: 本文が非空、ログに 429 ヒットあり -> exit_code は変わらず、warnings にレート制限の記載が入る。error は nil のまま
func TestB1_BodyNonEmptyWith429_ExitCodeUnchanged_WarnsRateLimit(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello from agy")
	t.Setenv("STUB_LOG_EXTRA", "HTTP 429 resource_exhausted")
	t.Setenv("STUB_EXIT", "0")

	rc, out, errb := run(t, home, []string{"do work"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	if r.Error != nil {
		t.Errorf("error should be nil when body is non-empty, got %+v", r.Error)
	}
	if r.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", r.ExitCode)
	}
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "レート制限") || strings.Contains(w, "429") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected rate-limit warning in r.Warnings, got %v", r.Warnings)
	}
	if !strings.Contains(errb, "warning:") {
		t.Errorf("expected warning on stderr, got %q", errb)
	}
}

// B2: 本文がゼロ、ログに 429 ヒットあり -> 従来どおり error.reason = "rate_limited" / exit 42
func TestB2_BodyEmptyWith429_ExitCode42RateLimited(t *testing.T) {
	home := newHome(t)
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

// B3: 本文が非空、ログに 429 ヒットなし -> warnings にレート制限の行が増えない
func TestB3_BodyNonEmptyNo429_NoRateLimitWarning(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello from agy")
	t.Setenv("STUB_LOG_EXTRA", "all good")
	t.Setenv("STUB_EXIT", "0")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	for _, w := range r.Warnings {
		if strings.Contains(w, "レート制限") || strings.Contains(w, "429") {
			t.Errorf("unexpected rate-limit warning in r.Warnings: %v", w)
		}
	}
}

// B4: 本文が非空、ログが読めない -> 警告は出ず、実行結果は従来どおり
func TestB4_BodyNonEmptyUnreadableLog_NoRateLimitWarning(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello from agy")
	t.Setenv("STUB_EXIT", "0")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	for _, w := range r.Warnings {
		if strings.Contains(w, "レート制限") || strings.Contains(w, "429") {
			t.Errorf("unexpected rate-limit warning when log has no 429: %v", w)
		}
	}
}

// B5: 本文が非空、429 ヒットあり、既に別の warning がある -> 既存の warning が消えず、レート制限の行が追加される
func TestB5_BodyNonEmptyWith429_ExistingWarningPreserved(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello from agy")
	t.Setenv("STUB_LOG_EXTRA", "error executing cascade step: CORTEX_STEP_TYPE_RUN_COMMAND: failed\nHTTP 429 resource_exhausted")
	t.Setenv("STUB_EXIT", "0")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	hasExisting := false
	hasRateLimit := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "RUN_COMMAND") {
			hasExisting = true
		}
		if strings.Contains(w, "レート制限") || strings.Contains(w, "429") {
			hasRateLimit = true
		}
	}
	if !hasExisting {
		t.Errorf("existing warning (RUN_COMMAND) was lost: %v", r.Warnings)
	}
	if !hasRateLimit {
		t.Errorf("rate-limit warning was not added: %v", r.Warnings)
	}
}

// 追加テスト: TestAdd_BodyNonEmptyMultiple429Hits_WarningCountMatches / 複数429ヒット時にヒット件数が警告文に正しく入るかの検証
func TestAdd_BodyNonEmptyMultiple429Hits_WarningCountMatches(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "hello from agy")
	t.Setenv("STUB_LOG_EXTRA", "HTTP 429 resource_exhausted\nHTTP 429 resource_exhausted\nHTTP 429 resource_exhausted")
	t.Setenv("STUB_EXIT", "0")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "レート制限シグネチャのヒット数: 3") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning with レート制限シグネチャのヒット数: 3, got warnings: %v", r.Warnings)
	}
}

// 追加テスト: TestRunner_ToolStallWarning / runner 全体で同一ツールの空転警告が warnings に追加されることの検証
func TestRunner_ToolStallWarning(t *testing.T) {
	home := newHome(t)
	logDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", stubConv, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	t0 := time.Now().Add(time.Second)
	for i := 0; i < 12; i++ {
		curTime := t0.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"custom_tool"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, i+1)
	}
	_ = os.WriteFile(filepath.Join(logDir, "transcript.jsonl"), []byte(sb.String()), 0o600)

	t.Setenv("STUB_BODY", "some output")
	t.Setenv("STUB_EXIT", "0")
	t.Setenv("AGY_RUN_TOOL_REPEAT_LIMIT", "10")

	rc, out, _ := run(t, home, []string{"do work"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "同一ツール (custom_tool)") && strings.Contains(w, "12 回") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected stall warning in runner warnings, got warnings: %v", r.Warnings)
	}
}

// TestD7_StallExitStillDetectsToolRepeat: 同一ツールが閾値超で繰り返され成果物0件、stall(exit 44)で終了時にwarningsが入ることの検証
func TestD7_StallExitStillDetectsToolRepeat(t *testing.T) {
	home := newHome(t)
	logDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", stubConv, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	t0 := time.Now().Add(time.Second)
	for i := 0; i < 12; i++ {
		curTime := t0.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"custom_tool"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, i+1)
	}
	_ = os.WriteFile(filepath.Join(logDir, "transcript.jsonl"), []byte(sb.String()), 0o600)

	t.Setenv("STUB_BODY", "thinking...")
	t.Setenv("STUB_SLEEP", "10")
	t.Setenv("AGY_RUN_IDLE_LIMIT", "1")
	t.Setenv("AGY_RUN_POLL", "1")
	t.Setenv("AGY_RUN_TOOL_REPEAT_LIMIT", "10")

	rc, out, _ := run(t, home, []string{"long task"})
	if rc != config.ExitStall {
		t.Fatalf("rc = %d, want 44 (stall)", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "stall" {
		t.Errorf("expected stall error, got: %+v", r.Error)
	}
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "同一ツール (custom_tool)") && strings.Contains(w, "12 回") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected stall warning in warnings on stall exit, got warnings: %v", r.Warnings)
	}
}

// TestD8_AbsTimeoutStillDetectsToolRepeat: 同一ツールが閾値超で繰り返され成果物0件、abs_timeout(exit 124)で終了時にwarningsが入ることの検証
func TestD8_AbsTimeoutStillDetectsToolRepeat(t *testing.T) {
	home := newHome(t)
	logDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", stubConv, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	t0 := time.Now().Add(time.Second)
	for i := 0; i < 12; i++ {
		curTime := t0.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"custom_tool"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, i+1)
	}
	_ = os.WriteFile(filepath.Join(logDir, "transcript.jsonl"), []byte(sb.String()), 0o600)

	t.Setenv("STUB_BODY", "thinking...")
	t.Setenv("STUB_SLEEP", "10")
	t.Setenv("AGY_RUN_IDLE_LIMIT", "100")
	t.Setenv("AGY_RUN_ABS_LIMIT", "1")
	t.Setenv("AGY_RUN_POLL", "1")
	t.Setenv("AGY_RUN_TOOL_REPEAT_LIMIT", "10")

	rc, out, _ := run(t, home, []string{"long task"})
	if rc != config.ExitAbsTimeout {
		t.Fatalf("rc = %d, want 124 (abs_timeout)", rc)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "timeout" {
		t.Errorf("expected timeout error, got: %+v", r.Error)
	}
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "同一ツール (custom_tool)") && strings.Contains(w, "12 回") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected stall warning in warnings on abs_timeout exit, got warnings: %v", r.Warnings)
	}
}

func TestE9(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_LOG_EXTRA", "HTTP 429 Too Many Requests")

	rc, out, _ := run(t, home, []string{"hi"})
	if rc != config.ExitRateLimited {
		t.Fatalf("TestE9: rc = %d, want %d (ExitRateLimited)", rc, config.ExitRateLimited)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "rate_limited" {
		t.Errorf("TestE9: expected rate_limited error, got %+v", r.Error)
	}
}

func TestE10(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_LOG_EXTRA", "I0823 11:43:26.632945 156 quota_manager.go:45] doRefreshQuota: starting reload (force=true)\nW0823 11:43:23.208429 66 cache.go:123] cache update failed\nconfig.go:80] quotaProject=my-project-123")

	rc, out, _ := run(t, home, []string{"hi"})
	if rc == config.ExitRateLimited {
		t.Fatalf("TestE10: rc = %d, want not %d (should not be ExitRateLimited)", rc, config.ExitRateLimited)
	}
	r := parseResult(t, out)
	if r.Error != nil && r.Error.Reason == "rate_limited" {
		t.Errorf("TestE10: should not be rate_limited, got %+v", r.Error)
	}
}

// Case 5: 実行中に一致行 + 本文 0 バイトなら kill して exit 45
// 前提: newHome の stub agy。STUB_LOG_EXTRA = fixture 行、STUB_BODY 未設定（空）、STUB_SLEEP=10、AGY_RUN_POLL=1、AGY_RUN_IDLE_LIMIT=6
// 手順: run(t, home, ["long task"]) を実行し所要時間を測る
// 期待挙動: rc 45。結果 JSON の error.reason == "auth_required"、error.detail に triggering interactive OAuth を含む、error.hint に excludedCommands を含む。所要時間が 5 秒未満（idle limit 6 秒より前に止まった＝stall ではなく Probe で止まった証拠）。kill stamp（.agy-run.last-kill）が作られる。index.jsonl に "error_class":"auth_required" と "exit_code":45
// 検証観点: 実行中検知が stall より先に効くこと。停止経路が stall と同じ後始末（kill stamp）を通ること
func TestCase5_RunWatchdogKillsAuthRequired(t *testing.T) {
	home := newHome(t)
	const fixture = "ERROR: logging before google.Init: I0907 21:31:30.280284       1 printmode.go:387] Print mode: triggering interactive OAuth"
	t.Setenv("STUB_LOG_EXTRA", fixture)
	// STUB_BODY 未設定（空）
	t.Setenv("STUB_SLEEP", "10")
	t.Setenv("AGY_RUN_POLL", "1")
	t.Setenv("AGY_RUN_IDLE_LIMIT", "6")

	t0 := time.Now()
	rc, out, _ := run(t, home, []string{"long task"})
	elapsed := time.Since(t0)

	if rc != config.ExitAuthRequired {
		t.Fatalf("rc = %d, want %d (ExitAuthRequired)", rc, config.ExitAuthRequired)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "auth_required" {
		t.Fatalf("expected auth_required error, got %+v", r.Error)
	}
	if !strings.Contains(r.Error.Detail, "triggering interactive OAuth") {
		t.Errorf("error.detail = %q, want containing %q", r.Error.Detail, "triggering interactive OAuth")
	}
	if !strings.Contains(r.Error.Hint, "excludedCommands") {
		t.Errorf("error.hint = %q, want containing %q", r.Error.Hint, "excludedCommands")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("elapsed = %v, want < 5s (Probe stopped before 6s idle limit)", elapsed)
	}

	killStamp := filepath.Join(home, ".gemini", "antigravity-cli", ".agy-run.last-kill")
	if _, err := os.Stat(killStamp); err != nil {
		t.Errorf("kill stamp %s not created: %v", killStamp, err)
	}

	idxFile := filepath.Join(home, "data", "agy-run", "index.jsonl")
	data, err := os.ReadFile(idxFile)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	idxStr := string(data)
	if !strings.Contains(idxStr, `"error_class":"auth_required"`) {
		t.Errorf("index.jsonl does not contain error_class:auth_required: %s", idxStr)
	}
	if !strings.Contains(idxStr, `"exit_code":45`) {
		t.Errorf("index.jsonl does not contain exit_code:45: %s", idxStr)
	}
}

// Case 6: agy が自力終了した後の分類でも auth_required になる
// 前提: STUB_LOG_EXTRA = fixture 行、STUB_BODY 空、STUB_EXIT=1、STUB_SLEEP 未設定
// 手順: run(t, home, ["x"])
// 期待挙動: rc 45、error.reason == "auth_required"（agy_error ではない）
// 検証観点: print-timeout 等で agy が先に死んだケースでも分類が揃うこと
func TestCase6_RunPostExitAuthRequired(t *testing.T) {
	home := newHome(t)
	const fixture = "ERROR: logging before google.Init: I0907 21:31:30.280284       1 printmode.go:387] Print mode: triggering interactive OAuth"
	t.Setenv("STUB_LOG_EXTRA", fixture)
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_EXIT", "1")
	// STUB_SLEEP 未設定

	rc, out, _ := run(t, home, []string{"x"})
	if rc != config.ExitAuthRequired {
		t.Fatalf("rc = %d, want %d (ExitAuthRequired)", rc, config.ExitAuthRequired)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "auth_required" {
		t.Fatalf("expected auth_required error, got %+v", r.Error)
	}
}

// Case 7: 本文が非空なら kill も分類変更もせず warnings に載せる
// 前提: STUB_LOG_EXTRA = fixture 行、STUB_BODY="answer"、STUB_EXIT=0
// 手順: run(t, home, ["x"])
// 期待挙動: rc 0、error == nil、warnings のいずれかに interactive OAuth を含む
// 検証観点: 文字列マッチ単独で exit を動かさない（ADR 0001）
func TestCase7_RunNonEmptyBodyWithLogMatchesWarnings(t *testing.T) {
	home := newHome(t)
	const fixture = "ERROR: logging before google.Init: I0907 21:31:30.280284       1 printmode.go:387] Print mode: triggering interactive OAuth"
	t.Setenv("STUB_LOG_EXTRA", fixture)
	t.Setenv("STUB_BODY", "answer")
	t.Setenv("STUB_EXIT", "0")

	rc, out, _ := run(t, home, []string{"x"})
	if rc != config.ExitOK {
		t.Fatalf("rc = %d, want 0", rc)
	}
	r := parseResult(t, out)
	if r.Error != nil {
		t.Fatalf("expected error == nil, got %+v", r.Error)
	}
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "interactive OAuth") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning containing 'interactive OAuth', got warnings: %v", r.Warnings)
	}
}

// Case 8: auth_required はモデル fallback を起こさない
// 前提: Case 6 と同じ + -m gemini-3.5-flash-high,gemini-3.1-pro-high、STUB_COUNT_FILE を設定
// 手順: run(t, home, ["-m", "...", "x"])
// 期待挙動: rc 45、STUB_COUNT_FILE の値が 1、error.models_tried が空
// 検証観点: exit 42 の再試行契約を汚さないこと（ADR 0001「壊すと危ない前提」）
func TestCase8_RunAuthRequiredNoModelFallback(t *testing.T) {
	home := newHome(t)
	const fixture = "ERROR: logging before google.Init: I0907 21:31:30.280284       1 printmode.go:387] Print mode: triggering interactive OAuth"
	t.Setenv("STUB_LOG_EXTRA", fixture)
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_EXIT", "1")
	cntFile := filepath.Join(home, "stub_count")
	t.Setenv("STUB_COUNT_FILE", cntFile)

	rc, out, _ := run(t, home, []string{"-m", "gemini-3.5-flash-high,gemini-3.1-pro-high", "x"})
	if rc != config.ExitAuthRequired {
		t.Fatalf("rc = %d, want %d (ExitAuthRequired)", rc, config.ExitAuthRequired)
	}
	cntData, err := os.ReadFile(cntFile)
	if err != nil {
		t.Fatalf("read count file: %v", err)
	}
	if strings.TrimSpace(string(cntData)) != "1" {
		t.Errorf("STUB_COUNT_FILE = %q, want 1", string(cntData))
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "auth_required" {
		t.Fatalf("expected auth_required error, got %+v", r.Error)
	}
	if len(r.Error.ModelsTried) != 0 {
		t.Errorf("models_tried = %v, want empty", r.Error.ModelsTried)
	}
}

// Case 9: 一致行と 429 が同時にあり本文 0 バイトなら auth_required が勝つ
// 前提: STUB_LOG_EXTRA = fixture 行 + 改行 + HTTP 429 resource_exhausted. Resets in 0h0m1s、STUB_BODY 空、STUB_EXIT=0
// 手順: run(t, home, ["x"])
// 期待挙動: rc 45、error.reason == "auth_required"
// 検証観点: 分類の優先順位（auth_required → rate_limited）が実装に固定されていること
func TestCase9_AuthRequiredPrecedesRateLimited(t *testing.T) {
	home := newHome(t)
	const fixture = "ERROR: logging before google.Init: I0907 21:31:30.280284       1 printmode.go:387] Print mode: triggering interactive OAuth"
	t.Setenv("STUB_LOG_EXTRA", fixture+"\nHTTP 429 resource_exhausted. Resets in 0h0m1s")
	t.Setenv("STUB_BODY", "")
	t.Setenv("STUB_EXIT", "0")

	rc, out, _ := run(t, home, []string{"x"})
	if rc != config.ExitAuthRequired {
		t.Fatalf("rc = %d, want %d (ExitAuthRequired)", rc, config.ExitAuthRequired)
	}
	r := parseResult(t, out)
	if r.Error == nil || r.Error.Reason != "auth_required" {
		t.Fatalf("expected auth_required error, got %+v", r.Error)
	}
}

// 追加テスト: TestAuthProbe_NonEmptyBodyAfterMatchReturnsNone / TOCTOU 防止: 一致行走査後に本文サイズを確認し非空なら None を返す判定順序の検証
func TestAuthProbe_NonEmptyBodyAfterMatchReturnsNone(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agy.log")
	const fixture = "ERROR: logging before google.Init: I0907 21:31:30.280284       1 printmode.go:387] Print mode: triggering interactive OAuth\n"
	if err := os.WriteFile(logPath, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	bodyNonEmpty := filepath.Join(dir, "body_nonempty")
	if err := os.WriteFile(bodyNonEmpty, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	bodyEmpty := filepath.Join(dir, "body_empty")
	if err := os.WriteFile(bodyEmpty, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	bodyMissing := filepath.Join(dir, "body_missing")
	logMissing := filepath.Join(dir, "log_missing")

	// 1. 1 バイト以上の本文ファイル -> None
	if got := authProbe(logPath, bodyNonEmpty); got != watchdog.None {
		t.Errorf("authProbe(log, bodyNonEmpty) = %v, want None", got)
	}

	// 2. 本文ファイルが 0 バイト -> AuthRequired
	if got := authProbe(logPath, bodyEmpty); got != watchdog.AuthRequired {
		t.Errorf("authProbe(log, bodyEmpty) = %v, want AuthRequired", got)
	}

	// 3. 本文ファイル不在 -> AuthRequired
	if got := authProbe(logPath, bodyMissing); got != watchdog.AuthRequired {
		t.Errorf("authProbe(log, bodyMissing) = %v, want AuthRequired", got)
	}

	// 4. ログ不在 -> None
	if got := authProbe(logMissing, bodyEmpty); got != watchdog.None {
		t.Errorf("authProbe(logMissing, bodyEmpty) = %v, want None", got)
	}
}

func waitPIDFilePoll(t *testing.T, path string, interval, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(interval)
	}
	t.Fatalf("pid file %s was never populated within %s", path, timeout)
	return 0
}

func readLastIndexRecord(t *testing.T, home string) index.Record {
	t.Helper()
	idxPath := filepath.Join(home, "data", "agy-run", "index.jsonl")
	data, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read index file %s: %v", idxPath, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("index file is empty: %s", idxPath)
	}
	var rec index.Record
	last := lines[len(lines)-1]
	if err := json.Unmarshal([]byte(last), &rec); err != nil {
		t.Fatalf("unmarshal last index record %q: %v", last, err)
	}
	return rec
}

func testSignalCancel(t *testing.T, sig syscall.Signal, wantCode int, wantDetail string) {
	t.Helper()
	home := newHome(t)
	pidFile := filepath.Join(t.TempDir(), "stub.pid")
	t.Setenv("STUB_SLEEP", "30")
	t.Setenv("STUB_PID_FILE", pidFile)

	sigCh := make(chan os.Signal, 1)
	cfg, _, err := config.Parse([]string{"--no-sandbox", "do task"}, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out, errb bytes.Buffer
	opt := Options{
		Stdout:  &out,
		Stderr:  &errb,
		Home:    home,
		Getenv:  os.Getenv,
		Signals: sigCh,
	}

	start := time.Now()
	type runResult struct {
		rc int
	}
	done := make(chan runResult, 1)
	go func() {
		rc := Run(cfg, opt)
		done <- runResult{rc: rc}
	}()

	pid := waitPIDFilePoll(t, pidFile, 50*time.Millisecond, 5*time.Second)

	sigCh <- sig

	var res runResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s after signal")
	}
	elapsed := time.Since(start)
	if elapsed >= 10*time.Second {
		t.Errorf("Run took %v, want < 10s", elapsed)
	}

	if res.rc != wantCode {
		t.Errorf("rc = %d, want %d", res.rc, wantCode)
	}

	r := parseResult(t, out.String())
	if r.ExitCode != wantCode {
		t.Errorf("result exit_code = %d, want %d", r.ExitCode, wantCode)
	}
	if r.Error == nil {
		t.Fatalf("result error is nil, want reason=%q", "cancelled_by_signal")
	}
	if r.Error.Reason != "cancelled_by_signal" {
		t.Errorf("error.reason = %q, want %q", r.Error.Reason, "cancelled_by_signal")
	}
	if r.Error.Detail != wantDetail {
		t.Errorf("error.detail = %q, want %q", r.Error.Detail, wantDetail)
	}

	rec := readLastIndexRecord(t, home)
	if rec.ErrorClass != "cancelled_by_signal" {
		t.Errorf("index error_class = %q, want %q", rec.ErrorClass, "cancelled_by_signal")
	}
	if rec.ExitCode != wantCode {
		t.Errorf("index exit_code = %d, want %d", rec.ExitCode, wantCode)
	}

	deadWithin(t, pid, 3*time.Second)
}

// Case 1: SIGTERM 受信で cancelled_by_signal / 143
func TestCase1_SIGTERM(t *testing.T) {
	testSignalCancel(t, syscall.SIGTERM, 143, "SIGTERM")
}

// Case 2: SIGINT → 130 / SIGINT、SIGHUP → 129 / SIGHUP
func TestCase2_SIGINT_SIGHUP(t *testing.T) {
	cases := []struct {
		name       string
		sig        syscall.Signal
		wantCode   int
		wantDetail string
	}{
		{name: "SIGINT", sig: syscall.SIGINT, wantCode: 130, wantDetail: "SIGINT"},
		{name: "SIGHUP", sig: syscall.SIGHUP, wantCode: 129, wantDetail: "SIGHUP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testSignalCancel(t, tc.sig, tc.wantCode, tc.wantDetail)
		})
	}
}

// Case 3: シグナル受信は rate_limited より優先し、モデル fallback を起こさない
func TestCase3_SignalPriorityOverRateLimit(t *testing.T) {
	home := newHome(t)
	pidFile := filepath.Join(t.TempDir(), "stub.pid")
	countFile := filepath.Join(t.TempDir(), "stub.count")
	t.Setenv("STUB_SLEEP", "30")
	t.Setenv("STUB_PID_FILE", pidFile)
	t.Setenv("STUB_LOG_EXTRA", "HTTP 429 resource_exhausted. Resets in 0h0m1s")
	t.Setenv("STUB_COUNT_FILE", countFile)

	sigCh := make(chan os.Signal, 1)
	cfg, _, err := config.Parse([]string{"--no-sandbox", "-m", "gemini-3.5-flash-high,gemini-3.1-pro-high", "do task"}, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out, errb bytes.Buffer
	opt := Options{
		Stdout:  &out,
		Stderr:  &errb,
		Home:    home,
		Getenv:  os.Getenv,
		Signals: sigCh,
	}

	done := make(chan int, 1)
	go func() {
		done <- Run(cfg, opt)
	}()

	pid := waitPIDFilePoll(t, pidFile, 50*time.Millisecond, 5*time.Second)
	sigCh <- syscall.SIGTERM

	var rc int
	select {
	case rc = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s after SIGTERM")
	}

	if rc != 143 {
		t.Errorf("rc = %d, want 143", rc)
	}
	r := parseResult(t, out.String())
	if r.ExitCode != 143 {
		t.Errorf("result exit_code = %d, want 143", r.ExitCode)
	}
	if r.Error == nil || r.Error.Reason != "cancelled_by_signal" {
		t.Errorf("error.reason = %v, want cancelled_by_signal", r.Error)
	}
	if r.Error != nil && len(r.Error.ModelsTried) > 0 {
		t.Errorf("error.models_tried = %v, want empty", r.Error.ModelsTried)
	}

	cntBytes, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("read count file: %v", err)
	}
	cnt := strings.TrimSpace(string(cntBytes))
	if cnt != "1" {
		t.Errorf("stub invocation count = %q, want \"1\"", cnt)
	}

	deadWithin(t, pid, 3*time.Second)
}

// Case 4: 外から agy だけが SIGKILL された場合は 137 / agy_error / detail signal: killed
func TestCase4_ExternalSIGKILL(t *testing.T) {
	home := newHome(t)
	pidFile := filepath.Join(t.TempDir(), "stub.pid")
	t.Setenv("STUB_SLEEP", "30")
	t.Setenv("STUB_PID_FILE", pidFile)

	cfg, _, err := config.Parse([]string{"--no-sandbox", "do task"}, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out, errb bytes.Buffer
	opt := Options{
		Stdout:  &out,
		Stderr:  &errb,
		Home:    home,
		Getenv:  os.Getenv,
		Signals: nil,
	}

	done := make(chan int, 1)
	go func() {
		done <- Run(cfg, opt)
	}()

	pid := waitPIDFilePoll(t, pidFile, 50*time.Millisecond, 5*time.Second)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stub: %v", err)
	}

	var rc int
	select {
	case rc = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s after SIGKILL")
	}

	if rc != 137 {
		t.Errorf("rc = %d, want 137", rc)
	}
	r := parseResult(t, out.String())
	if r.ExitCode != 137 {
		t.Errorf("result exit_code = %d, want 137", r.ExitCode)
	}
	if r.Error == nil {
		t.Fatalf("result error is nil, want reason=%q", "agy_error")
	}
	if r.Error.Reason != "agy_error" {
		t.Errorf("error.reason = %q, want %q", r.Error.Reason, "agy_error")
	}
	if r.Error.Detail != "signal: killed" {
		t.Errorf("error.detail = %q, want %q", r.Error.Detail, "signal: killed")
	}

	rec := readLastIndexRecord(t, home)
	if rec.ErrorClass != "agy_error" {
		t.Errorf("index error_class = %q, want %q", rec.ErrorClass, "agy_error")
	}
	if rec.ExitCode != 137 {
		t.Errorf("index exit_code = %d, want 137", rec.ExitCode)
	}
}

func TestSignalName(t *testing.T) {
	tests := []struct {
		sig  os.Signal
		want string
	}{
		{sig: syscall.SIGINT, want: "SIGINT"},
		{sig: syscall.SIGTERM, want: "SIGTERM"},
		{sig: syscall.SIGHUP, want: "SIGHUP"},
		{sig: syscall.SIGUSR1, want: "signal 10"},
	}
	for _, tc := range tests {
		if got := signalName(tc.sig); got != tc.want {
			t.Errorf("signalName(%v) = %q, want %q", tc.sig, got, tc.want)
		}
	}
}

// Case 7: cmd.Wait 完了後のシグナルは結果を上書きしない
func TestCase7_SignalAfterWaitDoesNotOverwriteSuccess(t *testing.T) {
	home := newHome(t)
	t.Setenv("STUB_BODY", "ok")
	t.Setenv("STUB_EXIT", "0")

	sigCh := make(chan os.Signal, 1)
	cfg, _, err := config.Parse([]string{"--no-sandbox", "do task"}, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	afterWait = func(ctx context.Context) {
		sigCh <- syscall.SIGTERM
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if signalFromContext(ctx) != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("signal was not recorded in context within 2s")
	}
	t.Cleanup(func() { afterWait = nil })

	var out, errb bytes.Buffer
	opt := Options{
		Stdout:  &out,
		Stderr:  &errb,
		Home:    home,
		Getenv:  os.Getenv,
		Signals: sigCh,
	}

	rc := Run(cfg, opt)
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}

	r := parseResult(t, out.String())
	if r.ExitCode != 0 {
		t.Errorf("result exit_code = %d, want 0", r.ExitCode)
	}
	if !strings.Contains(r.Body, "ok") {
		t.Errorf("result body = %q, want containing \"ok\"", r.Body)
	}
	if r.Error != nil {
		t.Errorf("result error = %+v, want nil", r.Error)
	}

	rec := readLastIndexRecord(t, home)
	if rec.ExitCode != 0 {
		t.Errorf("index exit_code = %d, want 0", rec.ExitCode)
	}
	if rec.ErrorClass != "" {
		t.Errorf("index error_class = %q, want empty", rec.ErrorClass)
	}
}

// Case 8: cmd.Start 前にシグナル受信済みなら agy を起動しない
func TestCase8_SignalBeforeStartDoesNotSpawn(t *testing.T) {
	home := newHome(t)
	countFile := filepath.Join(t.TempDir(), "stub.count")
	t.Setenv("STUB_COUNT_FILE", countFile)

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM

	cfg, _, err := config.Parse([]string{"--no-sandbox", "do task"}, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out, errb bytes.Buffer
	opt := Options{
		Stdout:  &out,
		Stderr:  &errb,
		Home:    home,
		Getenv:  os.Getenv,
		Signals: sigCh,
	}

	rc := Run(cfg, opt)
	if rc != 143 {
		t.Errorf("rc = %d, want 143", rc)
	}

	r := parseResult(t, out.String())
	if r.ExitCode != 143 {
		t.Errorf("result exit_code = %d, want 143", r.ExitCode)
	}
	if r.Error == nil {
		t.Fatalf("result error is nil, want cancelled_by_signal")
	}
	if r.Error.Reason != "cancelled_by_signal" {
		t.Errorf("error.reason = %q, want %q", r.Error.Reason, "cancelled_by_signal")
	}
	if r.Error.Detail != "SIGTERM" {
		t.Errorf("error.detail = %q, want %q", r.Error.Detail, "SIGTERM")
	}

	if _, err := os.Stat(countFile); !os.IsNotExist(err) {
		t.Errorf("count file exists (stub was spawned), want not exist")
	}
}

// Case 9: --wait-quota 待機中のシグナルで即時終了する
func TestCase9_SignalDuringWaitQuotaExitsPromptly(t *testing.T) {
	home := newHome(t)
	countFile := filepath.Join(t.TempDir(), "stub.count")
	t.Setenv("STUB_RL_UNTIL", "1")
	t.Setenv("STUB_RESET", "0h0m20s")
	t.Setenv("STUB_COUNT_FILE", countFile)

	sigCh := make(chan os.Signal, 1)
	cfg, _, err := config.Parse([]string{"--no-sandbox", "-m", "gemini-3.5-flash-high", "--wait-quota", "do task"}, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	afterWait = func(ctx context.Context) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			sigCh <- syscall.SIGTERM
		}()
	}
	t.Cleanup(func() { afterWait = nil })

	var out, errb bytes.Buffer
	opt := Options{
		Stdout:  &out,
		Stderr:  &errb,
		Home:    home,
		Getenv:  os.Getenv,
		Signals: sigCh,
	}

	start := time.Now()
	done := make(chan int, 1)
	go func() {
		done <- Run(cfg, opt)
	}()

	var rc int
	select {
	case rc = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s during --wait-quota")
	}
	elapsed := time.Since(start)
	if elapsed >= 5*time.Second {
		t.Errorf("Run took %v, want < 5s", elapsed)
	}

	if rc != 143 {
		t.Errorf("rc = %d, want 143", rc)
	}

	r := parseResult(t, out.String())
	if r.ExitCode != 143 {
		t.Errorf("result exit_code = %d, want 143", r.ExitCode)
	}
	if r.Error == nil || r.Error.Reason != "cancelled_by_signal" {
		t.Errorf("error.reason = %v, want cancelled_by_signal", r.Error)
	}
	if r.Error != nil && r.Error.Detail != "SIGTERM" {
		t.Errorf("error.detail = %q, want SIGTERM", r.Error.Detail)
	}

	cntBytes, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("read count file: %v", err)
	}
	cnt := strings.TrimSpace(string(cntBytes))
	if cnt != "1" {
		t.Errorf("stub invocation count = %q, want \"1\"", cnt)
	}
}

// Case 10: fallback 判定前にシグナルを確認する
func TestCase10_SignalBeforeFallbackPreventsNextCandidate(t *testing.T) {
	home := newHome(t)
	countFile := filepath.Join(t.TempDir(), "stub.count")
	t.Setenv("STUB_RL_MODEL", "gemini-3.5-flash-high")
	t.Setenv("STUB_COUNT_FILE", countFile)

	sigCh := make(chan os.Signal, 1)
	cfg, _, err := config.Parse([]string{"--no-sandbox", "-m", "gemini-3.5-flash-high,gemini-3.1-pro-high", "do task"}, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	afterWait = func(ctx context.Context) {
		sigCh <- syscall.SIGTERM
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if signalFromContext(ctx) != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("signal was not recorded in context within 2s")
	}
	t.Cleanup(func() { afterWait = nil })

	var out, errb bytes.Buffer
	opt := Options{
		Stdout:  &out,
		Stderr:  &errb,
		Home:    home,
		Getenv:  os.Getenv,
		Signals: sigCh,
	}

	rc := Run(cfg, opt)
	if rc != 143 {
		t.Errorf("rc = %d, want 143", rc)
	}

	r := parseResult(t, out.String())
	if r.ExitCode != 143 {
		t.Errorf("result exit_code = %d, want 143", r.ExitCode)
	}
	if r.Error == nil || r.Error.Reason != "cancelled_by_signal" {
		t.Errorf("error.reason = %v, want cancelled_by_signal", r.Error)
	}
	if r.Error != nil && len(r.Error.ModelsTried) > 0 {
		t.Errorf("error.models_tried = %v, want empty", r.Error.ModelsTried)
	}

	cntBytes, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("read count file: %v", err)
	}
	cnt := strings.TrimSpace(string(cntBytes))
	if cnt != "1" {
		t.Errorf("stub invocation count = %q, want \"1\"", cnt)
	}
}

func readAllIndexRecords(t *testing.T, home string) []index.Record {
	t.Helper()
	idxPath := filepath.Join(home, "data", "agy-run", "index.jsonl")
	data, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read index file %s: %v", idxPath, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var recs []index.Record
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var r index.Record
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("unmarshal index record %q: %v", ln, err)
		}
		recs = append(recs, r)
	}
	return recs
}

// Case 11: 中断結果が index.jsonl の最終行に残る
func TestCase11_InterruptedRunAppendsIndexRecord(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, home string) (config.Config, <-chan os.Signal)
		wantModel string
	}{
		{
			name: "fallback_pre_check",
			setup: func(t *testing.T, home string) (config.Config, <-chan os.Signal) {
				countFile := filepath.Join(t.TempDir(), "stub.count")
				t.Setenv("STUB_RL_MODEL", "gemini-3.5-flash-high")
				t.Setenv("STUB_COUNT_FILE", countFile)

				sigCh := make(chan os.Signal, 1)
				cfg, _, err := config.Parse([]string{"--no-sandbox", "-m", "gemini-3.5-flash-high,gemini-3.1-pro-high", "do task"}, os.Getenv)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}

				afterWait = func(ctx context.Context) {
					sigCh <- syscall.SIGTERM
					deadline := time.Now().Add(2 * time.Second)
					for time.Now().Before(deadline) {
						if signalFromContext(ctx) != nil {
							return
						}
						time.Sleep(20 * time.Millisecond)
					}
					t.Error("signal was not recorded in context within 2s")
				}
				t.Cleanup(func() { afterWait = nil })
				return cfg, sigCh
			},
			wantModel: "gemini-3.5-flash-high",
		},
		{
			name: "wait_quota_sleep",
			setup: func(t *testing.T, home string) (config.Config, <-chan os.Signal) {
				countFile := filepath.Join(t.TempDir(), "stub.count")
				t.Setenv("STUB_RL_UNTIL", "1")
				t.Setenv("STUB_RESET", "0h0m20s")
				t.Setenv("STUB_COUNT_FILE", countFile)

				sigCh := make(chan os.Signal, 1)
				cfg, _, err := config.Parse([]string{"--no-sandbox", "-m", "gemini-3.5-flash-high", "--wait-quota", "do task"}, os.Getenv)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}

				afterWait = func(ctx context.Context) {
					go func() {
						time.Sleep(300 * time.Millisecond)
						sigCh <- syscall.SIGTERM
					}()
				}
				t.Cleanup(func() { afterWait = nil })
				return cfg, sigCh
			},
			wantModel: "gemini-3.5-flash-high",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := newHome(t)
			cfg, sigCh := tc.setup(t, home)

			var out, errb bytes.Buffer
			opt := Options{
				Stdout:  &out,
				Stderr:  &errb,
				Home:    home,
				Getenv:  os.Getenv,
				Signals: sigCh,
			}

			rc := Run(cfg, opt)
			if rc != 143 {
				t.Errorf("rc = %d, want 143", rc)
			}

			recs := readAllIndexRecords(t, home)
			if len(recs) != 2 {
				t.Fatalf("index records count = %d, want 2; got: %+v", len(recs), recs)
			}

			// 1つ前の行: error_class: "rate_limited", exit_code: 42
			if recs[0].ErrorClass != "rate_limited" {
				t.Errorf("first record error_class = %q, want \"rate_limited\"", recs[0].ErrorClass)
			}
			if recs[0].ExitCode != 42 {
				t.Errorf("first record exit_code = %d, want 42", recs[0].ExitCode)
			}

			// 最終行: error_class: "cancelled_by_signal", exit_code: 143, conversation_id: stubConv, model: wantModel
			last := recs[1]
			if last.ErrorClass != "cancelled_by_signal" {
				t.Errorf("last record error_class = %q, want \"cancelled_by_signal\"", last.ErrorClass)
			}
			if last.ExitCode != 143 {
				t.Errorf("last record exit_code = %d, want 143", last.ExitCode)
			}
			if last.ConversationID == nil || *last.ConversationID != stubConv {
				t.Errorf("last record conversation_id = %v, want %q", last.ConversationID, stubConv)
			}
			if last.Model != tc.wantModel {
				t.Errorf("last record model = %q, want %q", last.Model, tc.wantModel)
			}
		})
	}
}






