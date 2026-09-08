// Package watchdog implements the two-stage liveness guard that replaces the
// bash fixed wall-clock timeout:
//
//   - idle limit: if the heartbeat (latest mtime across the agy log, the body
//     capture, and — once resolved — the conversation transcript) goes stale
//     for longer than Idle, the run is killed as a stall (exit 44).
//   - absolute limit: if total elapsed exceeds Abs regardless of activity, the
//     run is killed (exit 124).
//
// Threshold rationale (measured): across 501 transcripts / 3150 step gaps the
// median gap was 0s, p90 2s, p99 3s, normal max ~62s (the lone 291s outlier was
// a 429 retry). A 10-minute idle limit is ~10x the normal worst case, so it
// rarely mis-kills yet still catches a genuine stall.
package watchdog

import (
	"context"
	"os"
	"time"
)

// Reason is why the watchdog killed the run (or None if it did not).
type Reason int

const (
	// None means the process exited before any limit was hit.
	None Reason = iota
	// Stall means the heartbeat went idle past the idle limit (exit 44).
	Stall
	// AbsTimeout means total elapsed exceeded the absolute limit (exit 124).
	AbsTimeout
	// AuthRequired means interactive OAuth was triggered while non-interactive (exit 45).
	AuthRequired
)

// Clock abstracts time for testing.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Watcher polls heartbeat sources and enforces the idle/absolute limits.
type Watcher struct {
	Start time.Time            // run start (basis for the absolute limit)
	Idle  time.Duration       // idle limit; <=0 disables the idle check
	Abs   time.Duration       // absolute limit; <=0 disables the absolute check
	Poll  time.Duration       // polling interval
	Paths func() []string     // current heartbeat file paths (may grow as conv id resolves)
	Clock Clock               // injected clock (nil -> real time)
	Probe func() Reason       // optional per-poll probe; returns non-None Reason to kill early
}

// Run blocks until ctx is cancelled (the process exited on its own) or a limit
// is hit. On a limit it calls kill and returns the reason; on ctx cancellation
// it returns None without calling kill.
func (w Watcher) Run(ctx context.Context, kill func()) Reason {
	clk := w.Clock
	if clk == nil {
		clk = realClock{}
	}
	poll := w.Poll
	if poll <= 0 {
		poll = time.Second
	}
	lastActive := w.Start
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return None
		case <-ticker.C:
			if w.Probe != nil {
				if r := w.Probe(); r != None {
					kill()
					return r
				}
			}
			if mt, ok := latestMTime(w.paths()); ok && mt.After(lastActive) {
				lastActive = mt
			}
			switch decide(clk.Now(), w.Start, lastActive, w.Idle, w.Abs) {
			case Stall:
				kill()
				return Stall
			case AbsTimeout:
				kill()
				return AbsTimeout
			}
		}
	}
}

func (w Watcher) paths() []string {
	if w.Paths == nil {
		return nil
	}
	return w.Paths()
}

// decide applies the idle-then-absolute ordering. Because Idle < Abs in normal
// use, a genuine stall trips the idle check first; the absolute check only fires
// when the run is still active (recent heartbeat) but has simply run too long.
func decide(now, start, lastActive time.Time, idle, abs time.Duration) Reason {
	if idle > 0 && now.Sub(lastActive) >= idle {
		return Stall
	}
	if abs > 0 && now.Sub(start) >= abs {
		return AbsTimeout
	}
	return None
}

// latestMTime returns the newest modification time among the existing paths.
// Missing/unstattable paths are skipped; ok is false when none exist.
func latestMTime(paths []string) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, p := range paths {
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if mt := info.ModTime(); !found || mt.After(latest) {
			latest = mt
			found = true
		}
	}
	return latest, found
}
