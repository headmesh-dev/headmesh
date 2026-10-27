package limits

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestConcurrencyPerClient(t *testing.T) {
	l := New(2, 1000, 20*time.Millisecond)
	ctx := context.Background()
	r1, err := l.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(ctx, "a"); !errors.Is(err, ErrTooManyConcurrent) {
		t.Fatalf("third call: %v", err)
	}
	// Other clients are unaffected.
	rb, err := l.Acquire(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	rb()
	r1()
	r1() // release is idempotent
	if got := l.InFlight("a"); got != 1 {
		t.Fatalf("in flight = %d", got)
	}
	r3, err := l.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	r2()
	r3()
}

func TestQueuedCallGetsSlot(t *testing.T) {
	l := New(1, 1000, time.Second)
	ctx := context.Background()
	r1, _ := l.Acquire(ctx, "a")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, err := l.Acquire(ctx, "a")
		if err != nil {
			t.Error(err)
			return
		}
		r()
	}()
	time.Sleep(20 * time.Millisecond)
	r1()
	wg.Wait()
}

func TestRate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(100, 3, time.Millisecond)
	l.now = func() time.Time { return now }
	ctx := context.Background()
	for i := range 3 {
		r, err := l.Acquire(ctx, "a")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		r()
	}
	if _, err := l.Acquire(ctx, "a"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("fourth call: %v", err)
	}
	now = now.Add(20 * time.Second) // refills one token (3/min)
	r, err := l.Acquire(ctx, "a")
	if err != nil {
		t.Fatalf("after refill: %v", err)
	}
	r()
}

func TestCanceledWhileQueued(t *testing.T) {
	l := New(1, 100, time.Second)
	r, _ := l.Acquire(context.Background(), "a")
	defer r()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
