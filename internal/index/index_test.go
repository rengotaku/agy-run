package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestAppendWritesOneLinePerCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "index.jsonl")
	Append(path, Record{Prompt: "one", ExitCode: 0, ConversationID: ConvPtr("c1")})
	Append(path, Record{Prompt: "two", ExitCode: 42})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("lines = %d, want 2", lines)
	}

	// First record: schema defaulted, conversation_id present, artifacts [].
	var rec map[string]any
	first := data[:indexOfNL(data)]
	if err := json.Unmarshal(first, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["schema"].(float64) != 1 {
		t.Errorf("schema = %v", rec["schema"])
	}
	if rec["conversation_id"] != "c1" {
		t.Errorf("conversation_id = %v", rec["conversation_id"])
	}
	if arts, ok := rec["artifacts"].([]any); !ok || len(arts) != 0 {
		t.Errorf("artifacts = %v, want []", rec["artifacts"])
	}
}

func TestAppendNullConversationWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	Append(path, Record{Prompt: "x", ConversationID: ConvPtr("")})
	data, _ := os.ReadFile(path)
	var rec map[string]any
	if err := json.Unmarshal(data[:indexOfNL(data)], &rec); err != nil {
		t.Fatal(err)
	}
	if rec["conversation_id"] != nil {
		t.Errorf("conversation_id = %v, want null", rec["conversation_id"])
	}
}

func TestAppendPerms600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	Append(path, Record{Prompt: "x"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestAppendSandboxFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	// Sandboxed run: sandbox=true and net_log present.
	Append(path, Record{Prompt: "sb", Sandbox: true, NetLog: "/x/net-access.jsonl"})
	// Un-sandboxed run: sandbox=false emitted, net_log omitted.
	Append(path, Record{Prompt: "plain"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	nl := indexOfNL(data)
	var sb, plain map[string]any
	if err := json.Unmarshal(data[:nl], &sb); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data[nl+1:len(data)-1], &plain); err != nil {
		t.Fatal(err)
	}

	if sb["sandbox"] != true {
		t.Errorf("sandboxed run: sandbox = %v, want true", sb["sandbox"])
	}
	if sb["net_log"] != "/x/net-access.jsonl" {
		t.Errorf("sandboxed run: net_log = %v", sb["net_log"])
	}
	// false must still be emitted (no omitempty) so it is distinguishable from
	// a pre-#13 record that lacked the field entirely.
	if v, ok := plain["sandbox"]; !ok || v != false {
		t.Errorf("plain run: sandbox = %v (ok=%v), want false present", v, ok)
	}
	if _, ok := plain["net_log"]; ok {
		t.Errorf("plain run: net_log should be omitted, got %v", plain["net_log"])
	}
}

func indexOfNL(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return len(b)
}

// lastLine returns the last non-empty line of a jsonl blob.
func lastLine(t *testing.T, data []byte) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("last line is not JSON: %v\n%s", err, lines[len(lines)-1])
	}
	return rec
}

// T1: a failure Record carrying ErrorClass persists "error_class" in the
// index.jsonl line (issue #22 — this is the main point of the issue: exit-1
// runs must be diagnosable from the index instead of being indistinguishable
// blanks).
func TestAppendRecordsErrorClass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	Append(path, Record{Prompt: "x", ExitCode: 1, ErrorClass: "rate_limited"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := lastLine(t, data)
	if rec["error_class"] != "rate_limited" {
		t.Errorf("error_class = %v, want rate_limited", rec["error_class"])
	}
}

// T2: a success Record (no ErrorClass set) must omit the "error_class" key
// entirely (omitempty) so existing readers (agy-review) see the same shape as
// before this issue — the key must not be present-but-empty.
func TestAppendOmitsErrorClassOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	Append(path, Record{Prompt: "ok", ExitCode: 0})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := lastLine(t, data)
	if _, ok := rec["error_class"]; ok {
		t.Errorf("error_class present = %v, want key absent on success", rec["error_class"])
	}
}

// T3: below the rotation threshold, Append behaves exactly as before — no .1
// generation appears and the record is appended in place.
func TestAppendNoRotationBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "100000")

	Append(path, Record{Prompt: "one"})
	Append(path, Record{Prompt: "two"})

	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatalf(".1 must not exist below threshold")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 2 {
		t.Fatalf("want 2 lines appended in place, got %q", data)
	}
}

// T4: once the current index.jsonl has reached the threshold, the next Append
// must rotate the old content into .1 and start a fresh index.jsonl containing
// only the new record — this is the "stop unbounded growth" behaviour the
// issue asks for. Both files must stay 0600.
func TestAppendRotatesAtThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "10")

	// Pad the current file past the (tiny) threshold with a dummy line.
	if err := os.WriteFile(path, []byte(`{"prompt":"old-padding-line-well-past-ten-bytes"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	Append(path, Record{Prompt: "new"})

	oldData, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf(".1 not created: %v", err)
	}
	if !strings.Contains(string(oldData), "old-padding-line") {
		t.Errorf(".1 content = %s, want the pre-rotation content", oldData)
	}
	newData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(newData), "\n") != 1 || !strings.Contains(string(newData), `"prompt":"new"`) {
		t.Errorf("new index.jsonl = %s, want exactly the new record", newData)
	}
	for _, p := range []string{path, path + ".1"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s perm = %o, want 600", p, perm)
		}
	}
}

// T5: with a full set of existing generations (.1/.2/.3), a rotation must
// shift them (dropping the oldest .3) rather than growing without bound.
func TestAppendRotatesExistingGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "10")

	write := func(suffix, content string) {
		if err := os.WriteFile(path+suffix, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("", `{"prompt":"current-well-past-ten-bytes"}`+"\n")
	write(".1", `{"prompt":"gen1"}`+"\n")
	write(".2", `{"prompt":"gen2"}`+"\n")
	write(".3", `{"prompt":"gen3-oldest"}`+"\n")

	Append(path, Record{Prompt: "new"})

	gen1, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gen1), "current-well-past-ten-bytes") {
		t.Errorf(".1 = %s, want the pre-rotation current content", gen1)
	}
	gen2, err := os.ReadFile(path + ".2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gen2), "gen1") {
		t.Errorf(".2 = %s, want the old .1 content", gen2)
	}
	gen3, err := os.ReadFile(path + ".3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gen3), "gen2") {
		t.Errorf(".3 = %s, want the old .2 content (gen3-oldest must be dropped)", gen3)
	}
	if strings.Contains(string(gen3), "gen3-oldest") {
		t.Errorf(".3 still holds the oldest generation, want it dropped: %s", gen3)
	}
}

// T6: when the rotation targets cannot be written (read-only parent dir), the
// best-effort contract must hold — Append never panics, and (since the
// pre-existing index.jsonl itself can still be opened for append without
// directory write permission) the record is still appended.
func TestAppendRotationFailureDoesNotPanic(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses DAC permissions")
	}
	dir := t.TempDir()
	defer func() { _ = os.Chmod(dir, 0o700) }() // let TestMain/TempDir cleanup remove it
	path := filepath.Join(dir, "index.jsonl")
	t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "10")

	if err := os.WriteFile(path, []byte(`{"prompt":"old-well-past-ten-bytes"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // read+execute only: renames/creates fail
		t.Fatal(err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Append panicked on rotation failure: %v", r)
			}
		}()
		Append(path, Record{Prompt: "new"})
	}()

	_ = os.Chmod(dir, 0o700)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "old-well-past-ten-bytes") || !strings.Contains(string(data), `"prompt":"new"`) {
		t.Errorf("index = %s, want the append to still land in the un-rotated file", data)
	}
}

// T7: AGY_RUN_INDEX_MAX_BYTES lets operators tune or disable rotation.
func TestAppendRotationThresholdEnvOverride(t *testing.T) {
	t.Run("small threshold triggers rotation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "index.jsonl")
		t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "1024")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		Append(path, Record{Prompt: "new"})

		if _, err := os.Stat(path + ".1"); err != nil {
			t.Fatalf("expected rotation with a 1024-byte threshold on a 2KB file: %v", err)
		}
	})

	t.Run("zero disables rotation even on a huge file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "index.jsonl")
		t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "0")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 1<<20)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		Append(path, Record{Prompt: "new"})

		if _, err := os.Stat(path + ".1"); err == nil {
			t.Fatalf(".1 must not exist when AGY_RUN_INDEX_MAX_BYTES=0 disables rotation")
		}
	})

	t.Run("negative value falls back to default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "index.jsonl")
		t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "-5")
		// Well under the real 10MB default, so a fallback means no rotation.
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		Append(path, Record{Prompt: "new"})

		if _, err := os.Stat(path + ".1"); err == nil {
			t.Fatalf(".1 must not exist: a negative threshold should fall back to the 10MB default, not disable/shrink rotation")
		}
	})

	t.Run("non-numeric value falls back to default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "index.jsonl")
		t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "abc")
		// Well under the real 10MB default, so a fallback means no rotation.
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		Append(path, Record{Prompt: "new"})

		if _, err := os.Stat(path + ".1"); err == nil {
			t.Fatalf(".1 must not exist: a non-numeric threshold should fall back to the 10MB default")
		}
	})
}

// TestAppendSkipsRotationWhenLockHeld (concurrent-rotation race, HIGH):
// serializes rotation via a non-blocking flock on "<path>.rotate.lock". While
// another process holds that lock, this Append must skip rotation for the
// round (no .1 created) rather than block, and the record itself must still
// be appended — the lock only ever gates the rotate step, never Append.
func TestAppendSkipsRotationWhenLockHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	t.Setenv("AGY_RUN_INDEX_MAX_BYTES", "10")

	if err := os.WriteFile(path, []byte(`{"prompt":"old-well-past-ten-bytes"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate a concurrent agy-run process already mid-rotation: hold the
	// same non-blocking flock this Append call would need.
	lf, err := os.OpenFile(path+".rotate.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lf.Close() }()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("failed to acquire the test's own rotation lock: %v", err)
	}

	Append(path, Record{Prompt: "new"})

	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatalf(".1 must not exist: rotation should be skipped while another holder has the lock")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "old-well-past-ten-bytes") || !strings.Contains(string(data), `"prompt":"new"`) {
		t.Errorf("index = %s, want the append to still succeed (old content retained, new record appended)", data)
	}
}
