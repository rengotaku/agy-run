package runner

import (
	"os"
	"syscall"
	"unsafe"
)

// isTTY reports whether fd refers to a terminal. A TCGETS ioctl succeeds only on
// a real tty; it fails (ENOTTY) on a pipe, a regular file, or /dev/null — which
// is exactly how a delegated/background run's stdin looks. (TCGETS/Termios are
// Linux-only symbols, hence this file's _linux build constraint.)
func isTTY(fd uintptr) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd,
		uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return errno == 0
}

// shouldDetach is the side-effect-free half of detachFromLauncher: a
// delegated/background run (stdin is a pipe or /dev/null, never a tty) should
// detach; an interactive foreground run (stdin is a tty) must not. Split out so
// the decision is unit-testable without performing the real Setpgid.
func shouldDetach(stdinFd uintptr) bool {
	return !isTTY(stdinFd)
}

// detachFromLauncher moves agy-run into its own process group so a process-group
// teardown aimed at the launcher — e.g. Claude's background Bash being killed
// with `kill -- -<pgid>` — no longer reaches agy-run. Without this, agy-run
// shares the launcher's process group, so the launcher's death signal is
// delivered to agy-run, whose #8 handler then correctly tears agy down too: the
// delegation dies mid-flight (issue #9).
//
// This is the counterpart to the #7/#8 fix, not a reversal of it. agy-run still
// reaps agy's whole process group when a signal is delivered to agy-run ITSELF
// (its idle/abs watchdog, or an explicit `kill <agy-run pid>`), so no agy orphan
// leaks. Detach only severs the *launcher-group* signal path.
//
// It is skipped when stdin is a terminal: an interactive foreground run must keep
// sharing the shell's foreground process group so Ctrl-C (SIGINT to that group)
// still reaches agy-run. It returns whether a detach was performed.
func detachFromLauncher() bool {
	if !shouldDetach(os.Stdin.Fd()) {
		return false
	}
	// Setpgid(0,0) makes the caller its own process-group leader. It is a no-op
	// when already a leader, and only fails (EPERM) for a session leader — which
	// agy-run, always launched as a child process, never is.
	if err := syscall.Setpgid(0, 0); err != nil {
		return false
	}
	return true
}
