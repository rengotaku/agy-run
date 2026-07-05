package slot

import (
	"testing"
	"time"
)

func TestAcquireAndRelease(t *testing.T) {
	dir := t.TempDir()
	h, ok, err := Acquire(dir, 2, 1)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if h.Index != 1 {
		t.Errorf("index = %d, want 1", h.Index)
	}
	h.Release()
	h.Release() // idempotent
}

func TestAcquireSecondSlotWhenFirstHeld(t *testing.T) {
	dir := t.TempDir()
	h1, ok, err := Acquire(dir, 2, 1)
	if err != nil || !ok {
		t.Fatalf("first: %v %v", ok, err)
	}
	defer h1.Release()
	h2, ok, err := Acquire(dir, 2, 1)
	if err != nil || !ok {
		t.Fatalf("second: %v %v", ok, err)
	}
	defer h2.Release()
	if h2.Index != 2 {
		t.Errorf("second index = %d, want 2", h2.Index)
	}
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }
func (f *fakeClock) Sleep(d time.Duration) {
	f.t = f.t.Add(d) // advance instead of sleeping so the deadline is reached fast
}

func TestAcquireTimesOutWhenFull(t *testing.T) {
	dir := t.TempDir()
	h1, ok, _ := Acquire(dir, 1, 1)
	if !ok {
		t.Fatal("first acquire failed")
	}
	defer h1.Release()

	clk := &fakeClock{t: time.Unix(1000, 0)}
	h2, ok, err := acquire(dir, 1, 2, clk)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatal("expected timeout when the only slot is held")
	}
	if h2 != nil {
		t.Error("handle should be nil on timeout")
	}
}
