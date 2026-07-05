// Package slot implements the bounded-parallel concurrency semaphore using
// per-slot flock files (mirrors the bash flock-on-N-slots design).
//
// Up to maxConc runs proceed without waiting; excess callers poll for a free
// slot until the deadline, then fail with ExitLockTimeout. The held lock is an
// open fd flock(LOCK_EX|LOCK_NB) — it releases automatically when the process
// exits, so a crash never leaves a slot wedged.
package slot

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Handle is an acquired slot. Keep it alive for the duration of the run; call
// Release (or just exit) to free the slot.
type Handle struct {
	Index int
	f     *os.File
}

// Release closes the underlying fd, freeing the flock.
func (h *Handle) Release() {
	if h != nil && h.f != nil {
		_ = h.f.Close()
		h.f = nil
	}
}

// Clock abstracts time for testability.
type Clock interface {
	Now() time.Time
	Sleep(time.Duration)
}

type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }

// Acquire tries to grab one of maxConc slots under dir, polling every second
// until waitSec elapses. On timeout it returns a nil Handle and ok=false.
func Acquire(dir string, maxConc, waitSec int) (*Handle, bool, error) {
	return acquire(dir, maxConc, waitSec, realClock{})
}

func acquire(dir string, maxConc, waitSec int, clk Clock) (*Handle, bool, error) {
	if maxConc < 1 {
		maxConc = 1
	}
	deadline := clk.Now().Add(time.Duration(waitSec) * time.Second)
	for {
		for i := 1; i <= maxConc; i++ {
			h, err := tryLock(dir, i)
			if err != nil {
				return nil, false, err
			}
			if h != nil {
				return h, true, nil
			}
		}
		if !clk.Now().Before(deadline) {
			return nil, false, nil
		}
		clk.Sleep(time.Second)
	}
}

// tryLock attempts a non-blocking flock on slot i. Returns (handle,nil) on
// success, (nil,nil) if the slot is busy, or (nil,err) on an unexpected error.
func tryLock(dir string, i int) (*Handle, error) {
	path := filepath.Join(dir, fmt.Sprintf(".agy-run.slot.%d", i))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, nil // busy — try next slot
		}
		return nil, err
	}
	return &Handle{Index: i, f: f}, nil
}
