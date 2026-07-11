package runner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rengotaku/agy-run/internal/config"
	"github.com/rengotaku/agy-run/internal/emit"
)

// pidAlive reports whether pid still exists (signal 0 only probes existence).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// waitPIDFile polls path until it holds a pid, failing after a short deadline.
func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s was never populated", path)
	return 0
}

// deadWithin fails unless pid disappears within d.
func deadWithin(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %s (orphan leak)", pid, d)
}

// grandchildStub is a stand-in agy that spawns a detached-looking grandchild
// (a background `sleep`), records its pid, then hangs. It lets a test assert the
// grandchild is reaped when the run is torn down.
func grandchildStub(gpidFile string) string {
	return "#!/bin/sh\n" +
		"if [ \"$1\" = models ]; then printf '%s\\n' 'Gemini 3.5 Flash (High)'; exit 0; fi\n" +
		"sleep 60 & echo $! > " + gpidFile + "\n" +
		"sleep 60\n"
}

// TestAttemptCancelReapsGroup (issue #7): cancelling the run context must kill
// agy's ENTIRE process group — the direct child and its grandchildren — so no
// descendant is orphaned to leak inotify watches. Without the group-kill
// cmd.Cancel wired into attempt, the background `sleep` grandchild survives.
func TestAttemptCancelReapsGroup(t *testing.T) {
	home := newHome(t)
	gpidFile := filepath.Join(t.TempDir(), "gpid")
	stub := grandchildStub(gpidFile)
	if err := os.WriteFile(filepath.Join(home, ".local", "bin", "agy"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := config.Parse([]string{"hang please"}, os.Getenv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	paths := DefaultPaths(home, os.Getenv)
	e := emit.New(io.Discard, io.Discard)
	opt := Options{Stdout: io.Discard, Stderr: io.Discard, Home: home, Getenv: os.Getenv}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		attempt(ctx, cfg, "", opt, e, paths)
		close(done)
	}()

	grand := waitPIDFile(t, gpidFile)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt did not return after context cancel")
	}
	deadWithin(t, grand, 3*time.Second)
}

// TestSIGTERMReapsAgyGroup (issue #7): the real end-to-end scenario. When the
// launcher terminates agy-run with SIGTERM while agy is running, agy-run must
// catch the signal and kill agy's whole process group before exiting, instead
// of leaving an orphaned agy (and its grandchildren) holding inotify watches.
func TestSIGTERMReapsAgyGroup(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}

	bin := filepath.Join(t.TempDir(), "agy-run-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "../.." // module root (the main package)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agy-run: %v\n%s", err, out)
	}

	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gpidFile := filepath.Join(t.TempDir(), "gpid")
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte(grandchildStub(gpidFile)), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "hang please")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_DATA_HOME="+filepath.Join(home, "data"))
	// Give agy-run its own process group so our SIGTERM targets only it (the
	// launcher-death scenario) and never the test runner.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agy-run: %v", err)
	}

	grand := waitPIDFile(t, gpidFile)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal agy-run: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatal("agy-run did not exit after SIGTERM")
	}

	deadWithin(t, grand, 3*time.Second)
}

// TestDetachSurvivesLauncherGroupTeardown (issue #9): when agy-run is launched
// inside the LAUNCHER's process group (as Claude's background Bash does) and that
// whole group is torn down (`kill -- -<launcher pgid>`), agy-run must have
// detached into its own process group and keep running to completion — the
// delegation must not die with the launcher. Counterpart to
// TestSIGTERMReapsAgyGroup, which asserts a signal to agy-run ITSELF still reaps
// agy (the #7/#8 orphan fix stays intact).
func TestDetachSurvivesLauncherGroupTeardown(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	bin := filepath.Join(t.TempDir(), "agy-run-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "../.." // module root (the main package)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agy-run: %v\n%s", err, out)
	}

	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gpidFile := filepath.Join(t.TempDir(), "gpid")
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte(grandchildStub(gpidFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	agyrunPidFile := filepath.Join(t.TempDir(), "agyrun_pid")

	// The launcher: a shell that starts agy-run as a background child in ITS OWN
	// process group (no per-child setpgid), mirroring Claude's background Bash.
	script := bin + " hang please & echo $! > " + agyrunPidFile + "\nwait\n"
	launcher := exec.Command("sh", "-c", script)
	launcher.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_DATA_HOME="+filepath.Join(home, "data"))
	// Give the launcher its own process group so we tear down THAT group (and only
	// it) — exactly the `kill -- -<pgid>` a background-task teardown performs.
	launcher.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := launcher.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	launcherPGID := launcher.Process.Pid // leader: pgid == pid
	defer func() { _ = syscall.Kill(-launcherPGID, syscall.SIGKILL) }()

	// Waiting for agy's grandchild guarantees agy-run has already spawned agy,
	// which happens long after detachFromLauncher() at the top of Run().
	grand := waitPIDFile(t, gpidFile)
	agyrunPid := waitPIDFile(t, agyrunPidFile)
	defer func() { _ = syscall.Kill(-agyrunPid, syscall.SIGKILL) }()

	// Tear down the launcher's whole process group.
	if err := syscall.Kill(-launcherPGID, syscall.SIGTERM); err != nil {
		t.Fatalf("kill launcher group: %v", err)
	}
	_, _ = launcher.Process.Wait()

	// The delegation must survive: agy-run detached, so the group teardown missed
	// it and agy (its grandchild) is still alive a beat later.
	time.Sleep(1 * time.Second)
	if !pidAlive(agyrunPid) {
		t.Fatalf("agy-run (pid %d) was killed by the launcher-group teardown — detach failed", agyrunPid)
	}
	if !pidAlive(grand) {
		t.Fatalf("agy (grandchild pid %d) died with the launcher — delegation was cut short", grand)
	}
}
