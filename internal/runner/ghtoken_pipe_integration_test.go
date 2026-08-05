package runner

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/rengotaku/agy-run/internal/sandbox"
)

// Case 5 (issue #25): the direct regression test for the vulnerability —
// GH_TOKEN must reach the sandboxed process via the fd/--file path
// (proving the transport actually works) while never appearing, as a string,
// anywhere in the bwrap exec.Cmd's own argv (proving the fix: bwrap's argv is
// exactly what /proc/<pid>/cmdline exposes to any same-uid process via
// `ps`/`pgrep -af`).
//
// This wires the pipe + sandbox.BuildArgs + sandbox.WrapExecWithSecretEnv the
// same way internal/runner/runner.go's attempt() does in production, so a
// regression in either function is caught here without needing to run the
// full attempt()/proxy/watchdog machinery.
func TestGHTokenReachesSandboxOnlyViaPipeNeverViaArgv(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not on PATH; skipping sandbox integration test")
	}

	const dummyToken = "dummy-secret-abc123"

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer func() { _ = pr.Close() }()
	go func() {
		_, _ = pw.WriteString(dummyToken)
		_ = pw.Close()
	}()

	sbOpt := sandbox.Options{
		Home:      t.TempDir(),
		PathEnv:   "/usr/bin:/bin",
		GHTokenFD: 3, // ExtraFiles[0] always lands at fd 3 in the child.
	}
	bwrapArgs := sandbox.BuildArgs(sbOpt)

	execArgv := sandbox.WrapExecWithSecretEnv(sandbox.GHTokenSecretPath, "GH_TOKEN", []string{"/usr/bin/env"})

	fullArgs := append(append([]string{}, bwrapArgs...), "--")
	fullArgs = append(fullArgs, execArgv...)

	cmd := exec.Command("bwrap", fullArgs...)
	cmd.ExtraFiles = []*os.File{pr}

	out, runErr := cmd.Output()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			t.Fatalf("bwrap run failed: %v\nstderr: %s", runErr, ee.Stderr)
		}
		t.Fatalf("bwrap run failed: %v", runErr)
	}

	// The transfer actually worked: GH_TOKEN=<dummyToken> shows up in the
	// sandboxed `env` output.
	if !strings.Contains(string(out), "GH_TOKEN="+dummyToken) {
		t.Errorf("sandboxed env output missing GH_TOKEN=%s\noutput: %s", dummyToken, out)
	}

	// The regression guard: the dummy token string must not appear anywhere
	// in the argv Go actually hands to execve(2) for the bwrap process — this
	// is exactly what /proc/<pid>/cmdline (and therefore `ps auxww` /
	// `pgrep -af bwrap`) would expose to any other same-uid process.
	for i, a := range cmd.Args {
		if strings.Contains(a, dummyToken) {
			t.Errorf("cmd.Args[%d] contains the dummy token value (argv leak): %q\nfull args: %v", i, a, cmd.Args)
		}
	}
}

// TestGHTokenSecretFileActuallyRemovedAfterExec is the direct regression test
// for the codex P2 finding on issue #25's PR: --ro-bind-data creates a bind
// mount, and `rm -f` on a mount point always fails with EBUSY ("Device or
// resource busy") regardless of ro/rw — so WrapExecWithSecretEnv's one-shot
// `rm -f GHTokenSecretPath` would silently no-op (its trailing `;` swallows
// the failure) and leave the secret file readable for the sandboxed
// process's whole lifetime. sandbox.BuildArgs now uses `--perms 0600 --file`
// instead, which writes a genuine tmpfs regular file that `rm -f` can
// actually remove. This test proves the removal really happens *inside the
// same sandbox instance* (checked before the sandbox's own tmpfs is torn
// down) rather than merely asserting bwrap's exit code.
func TestGHTokenSecretFileActuallyRemovedAfterExec(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not on PATH; skipping sandbox integration test")
	}

	const dummyToken = "dummy-secret-abc123"

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer func() { _ = pr.Close() }()
	go func() {
		_, _ = pw.WriteString(dummyToken)
		_ = pw.Close()
	}()

	sbOpt := sandbox.Options{
		Home:      t.TempDir(),
		PathEnv:   "/usr/bin:/bin",
		GHTokenFD: 3, // ExtraFiles[0] always lands at fd 3 in the child.
	}
	bwrapArgs := sandbox.BuildArgs(sbOpt)

	// The final exec target checks, from *inside* the sandbox, whether the
	// secret file WrapExecWithSecretEnv just `rm -f`'d is really gone.
	checkArgv := []string{"/bin/sh", "-c",
		"test -e " + sandbox.GHTokenSecretPath + " && echo EXISTS || echo GONE"}
	execArgv := sandbox.WrapExecWithSecretEnv(sandbox.GHTokenSecretPath, "GH_TOKEN", checkArgv)

	fullArgs := append(append([]string{}, bwrapArgs...), "--")
	fullArgs = append(fullArgs, execArgv...)

	cmd := exec.Command("bwrap", fullArgs...)
	cmd.ExtraFiles = []*os.File{pr}

	out, runErr := cmd.Output()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			t.Fatalf("bwrap run failed: %v\nstderr: %s", runErr, ee.Stderr)
		}
		t.Fatalf("bwrap run failed: %v", runErr)
	}

	got := strings.TrimSpace(string(out))
	if got != "GONE" {
		t.Fatalf("secret file %s was not removed after rm -f (got %q, want %q) — see issue #25 codex P2 finding on --ro-bind-data EBUSY",
			sandbox.GHTokenSecretPath, got, "GONE")
	}
}
