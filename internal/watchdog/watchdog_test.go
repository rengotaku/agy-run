package watchdog

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	base := time.Unix(1000, 0)
	cases := []struct {
		name                    string
		now, start, lastActive  time.Time
		idle, abs               time.Duration
		want                    Reason
	}{
		{"idle stall", base.Add(200 * time.Second), base, base, 100 * time.Second, 3600 * time.Second, Stall},
		{"active within idle", base.Add(50 * time.Second), base, base.Add(45 * time.Second), 100 * time.Second, 3600 * time.Second, None},
		{"abs timeout with recent activity", base.Add(3601 * time.Second), base, base.Add(3600 * time.Second), 100 * time.Second, 3600 * time.Second, AbsTimeout},
		{"idle preferred when both cross", base.Add(4000 * time.Second), base, base, 100 * time.Second, 3600 * time.Second, Stall},
		{"idle disabled", base.Add(4000 * time.Second), base, base, 0, 0, None},
	}
	for _, c := range cases {
		if got := decide(c.now, c.start, c.lastActive, c.idle, c.abs); got != c.want {
			t.Errorf("%s: decide = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLatestMTime(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := time.Now().Add(time.Hour)
	_ = os.Chtimes(b, newer, newer)

	mt, ok := latestMTime([]string{a, b, "", filepath.Join(dir, "missing")})
	if !ok {
		t.Fatal("expected ok")
	}
	if mt.Before(newer.Add(-time.Second)) {
		t.Errorf("latest = %v, want ~%v", mt, newer)
	}
	if _, ok := latestMTime([]string{filepath.Join(dir, "none")}); ok {
		t.Error("expected ok=false for missing files")
	}
}

// TestRunStall drives a stub heartbeat file that never updates; the watchdog
// must detect the stall and invoke kill, returning Stall (exit 44 path).
func TestRunStall(t *testing.T) {
	dir := t.TempDir()
	hb := filepath.Join(dir, "heartbeat")
	if err := os.WriteFile(hb, []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	var killed atomic.Int32
	w := Watcher{
		Start: time.Now(),
		Idle:  80 * time.Millisecond,
		Abs:   10 * time.Second,
		Poll:  10 * time.Millisecond,
		Paths: func() []string { return []string{hb} },
	}
	got := w.Run(context.Background(), func() { killed.Add(1) })
	if got != Stall {
		t.Fatalf("reason = %v, want Stall", got)
	}
	if killed.Load() == 0 {
		t.Error("kill was not called")
	}
}

// TestRunNoKillOnFreshHeartbeat updates the heartbeat continuously; the
// watchdog must NOT kill, and returns None when the context is cancelled
// (process exited normally).
func TestRunNoKillOnFreshHeartbeat(t *testing.T) {
	dir := t.TempDir()
	hb := filepath.Join(dir, "heartbeat")
	if err := os.WriteFile(hb, []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-tk.C:
				_ = os.Chtimes(hb, now, now)
			}
		}
	}()

	var killed atomic.Int32
	done := make(chan Reason, 1)
	w := Watcher{
		Start: time.Now(),
		Idle:  200 * time.Millisecond,
		Abs:   10 * time.Second,
		Poll:  20 * time.Millisecond,
		Paths: func() []string { return []string{hb} },
	}
	go func() { done <- w.Run(ctx, func() { killed.Add(1) }) }()

	time.Sleep(500 * time.Millisecond) // longer than Idle; fresh heartbeat keeps it alive
	cancel()
	close(stop)

	if got := <-done; got != None {
		t.Errorf("reason = %v, want None", got)
	}
	if killed.Load() != 0 {
		t.Errorf("kill called %d times, want 0", killed.Load())
	}
}

// TestRunAbsTimeout keeps the heartbeat fresh but exceeds the absolute limit.
func TestRunAbsTimeout(t *testing.T) {
	dir := t.TempDir()
	hb := filepath.Join(dir, "heartbeat")
	var killed atomic.Int32
	w := Watcher{
		Start: time.Now(),
		Idle:  10 * time.Second,
		Abs:   60 * time.Millisecond,
		Poll:  10 * time.Millisecond,
		Paths: func() []string {
			now := time.Now()
			_ = os.WriteFile(hb, []byte("x"), 0o600)
			_ = os.Chtimes(hb, now, now)
			return []string{hb}
		},
	}
	if got := w.Run(context.Background(), func() { killed.Add(1) }); got != AbsTimeout {
		t.Fatalf("reason = %v, want AbsTimeout", got)
	}
	if killed.Load() == 0 {
		t.Error("kill not called")
	}
}

// Case 4: Watcher.Probe が None 以外を返したら kill してその Reason を返す
// 前提: Watcher{Idle: 0, Abs: 0, Poll: 10ms, Probe: <1 回目は None、2 回目以降 AuthRequired を返す関数>}。kill は呼び出し回数を数える
// 手順: Run(ctx, kill)
// 期待挙動: 戻り値 AuthRequired、kill が 1 回呼ばれる。同じ Watcher で Probe: nil にすると ctx cancel まで None（既存挙動）
// 検証観点: Probe の配線と、nil 時の後方互換
func TestCase4_WatcherProbe(t *testing.T) {
	var probeCalls atomic.Int32
	probe := func() Reason {
		if probeCalls.Add(1) == 1 {
			return None
		}
		return AuthRequired
	}

	var killed atomic.Int32
	w := Watcher{
		Idle:  0,
		Abs:   0,
		Poll:  10 * time.Millisecond,
		Probe: probe,
	}

	got := w.Run(context.Background(), func() { killed.Add(1) })
	if got != AuthRequired {
		t.Fatalf("Run reason = %v, want AuthRequired", got)
	}
	if killed.Load() != 1 {
		t.Fatalf("kill called %d times, want 1", killed.Load())
	}

	// 同じ Watcher で Probe: nil にすると ctx cancel まで None（既存挙動）
	ctx, cancel := context.WithCancel(context.Background())
	var killedNil atomic.Int32
	wNil := Watcher{
		Idle:  0,
		Abs:   0,
		Poll:  10 * time.Millisecond,
		Probe: nil,
	}
	done := make(chan Reason, 1)
	go func() {
		done <- wNil.Run(ctx, func() { killedNil.Add(1) })
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	gotNil := <-done
	if gotNil != None {
		t.Fatalf("Run with nil Probe reason = %v, want None", gotNil)
	}
	if killedNil.Load() != 0 {
		t.Fatalf("kill called %d times with nil Probe, want 0", killedNil.Load())
	}
}
