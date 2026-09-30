package applylock

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestOneWriterAtATime(t *testing.T) {
	at := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	l := NewWithClock(func() time.Time { return at })
	if l.Busy() != "" {
		t.Fatal("a new lock is busy")
	}
	g, err := l.TryAcquire(Groups, "groups-7")
	if err != nil {
		t.Fatal(err)
	}
	if !g.Active() || l.Busy() != "groups_pending" {
		t.Fatalf("active %v busy %q", g.Active(), l.Busy())
	}
	if info, ok := l.Current(); !ok || info != (Info{Holder: Groups, ID: "groups-7", Since: at}) {
		t.Fatalf("%+v %v", info, ok)
	}
	// Every other writer, and the same one again, is refused with the
	// holder's reason.
	for _, h := range []Holder{Plane, Update, Groups} {
		_, err := l.TryAcquire(h, "x")
		var b *BusyError
		if !errors.As(err, &b) || !errors.Is(err, ErrBusy) || b.Reason() != "groups_pending" || Reason(err) != "groups_pending" || b.ID != "groups-7" {
			t.Fatalf("%s: %v", h, err)
		}
	}
	g.Release()
	g.Release() // twice is fine
	if g.Active() || l.Busy() != "" {
		t.Fatal("not released")
	}
	p, err := l.TryAcquire(Plane, "a4-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	// A stale hold releases nothing.
	g.Release()
	if !p.Active() || l.Busy() != "plane_pending" {
		t.Fatal("a stale release freed someone else's hold")
	}
	if _, err := l.TryAcquire(Update, "u1"); Reason(err) != "plane_pending" {
		t.Fatalf("%v", err)
	}
	p.Release()
	u, err := l.TryAcquire(Update, "u1")
	if err != nil || l.Busy() != "update_pending" {
		t.Fatalf("%v %q", err, l.Busy())
	}
	u.Release()
	var none *Hold
	none.Release()
	if none.Active() || Reason(errors.New("x")) != "" {
		t.Fatal("nil hold")
	}
	if _, err := l.TryAcquire("", "x"); err == nil {
		t.Fatal("empty holder accepted")
	}
}

func TestConcurrentAcquireHasOneWinner(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := []Holder{Groups, Plane, Update}[i%3]
			if _, err := l.TryAcquire(h, fmt.Sprint(i)); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d winners", winners)
	}
}
