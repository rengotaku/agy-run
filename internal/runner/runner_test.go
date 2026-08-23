package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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



