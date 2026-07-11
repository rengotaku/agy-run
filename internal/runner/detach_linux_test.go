package runner

import (
	"os"
	"testing"
)

// TestShouldDetachNonTTY (issue #9): a delegated/background run's stdin is a pipe
// or /dev/null, never a tty, so shouldDetach must return true — the detach path.
// This exercises the decision without performing the real Setpgid side effect.
func TestShouldDetachNonTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	if isTTY(r.Fd()) {
		t.Fatal("isTTY(pipe) = true, want false")
	}
	if !shouldDetach(r.Fd()) {
		t.Fatal("shouldDetach(pipe) = false, want true (background run must detach)")
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer func() { _ = devnull.Close() }()
	if isTTY(devnull.Fd()) {
		t.Fatal("isTTY(/dev/null) = true, want false")
	}
	if !shouldDetach(devnull.Fd()) {
		t.Fatal("shouldDetach(/dev/null) = false, want true")
	}
}

// TestShouldDetachTTY (issue #9): the safety valve — an interactive foreground
// run (stdin is a tty) must NOT detach, so Ctrl-C keeps reaching agy-run. Skipped
// when no controlling terminal is available (e.g. CI), since we cannot fabricate
// a tty without one.
func TestShouldDetachTTY(t *testing.T) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no controlling terminal: %v", err)
	}
	defer func() { _ = tty.Close() }()
	if !isTTY(tty.Fd()) {
		t.Fatal("isTTY(/dev/tty) = false, want true")
	}
	if shouldDetach(tty.Fd()) {
		t.Fatal("shouldDetach(tty) = true, want false (interactive run must NOT detach)")
	}
}
