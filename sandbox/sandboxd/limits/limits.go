// Package limits enforces per-client concurrency (8) and rate (600/min)
// limits on tool calls.
package limits

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrRateLimited is returned when a client exceeds its call rate.
var ErrRateLimited = errors.New("rate limit exceeded")

// ErrTooManyConcurrent is returned when a client has too many calls in flight.
var ErrTooManyConcurrent = errors.New("too many concurrent calls")

// Limiter tracks every client independently.
type Limiter struct {
	maxConcurrent int
	perMinute     int
	queueWait     time.Duration
	now           func() time.Time

	mu      sync.Mutex
	clients map[string]*client
}

type client struct {
	sem     chan struct{}
	tokens  float64
	updated time.Time
}

// New returns a limiter. queueWait is how long a call waits for a free
// concurrency slot before failing.
func New(maxConcurrent, perMinute int, queueWait time.Duration) *Limiter {
	return &Limiter{
		maxConcurrent: maxConcurrent,
		perMinute:     perMinute,
		queueWait:     queueWait,
		now:           time.Now,
		clients:       map[string]*client{},
	}
}

func (l *Limiter) get(id string) *client {
	c, ok := l.clients[id]
	if !ok {
		c = &client{sem: make(chan struct{}, l.maxConcurrent), tokens: float64(l.perMinute), updated: l.now()}
		l.clients[id] = c
	}
	return c
}

// Acquire takes a rate token and a concurrency slot for id. Call the
// returned release when the call ends.
func (l *Limiter) Acquire(ctx context.Context, id string) (func(), error) {
	l.mu.Lock()
	c := l.get(id)
	now := l.now()
	refill := now.Sub(c.updated).Minutes() * float64(l.perMinute)
	c.tokens = min(float64(l.perMinute), c.tokens+refill)
	c.updated = now
	if c.tokens < 1 {
		wait := time.Duration((1 - c.tokens) / float64(l.perMinute) * float64(time.Minute))
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %d calls/minute for %q; retry in %s", ErrRateLimited, l.perMinute, id, wait.Round(time.Millisecond))
	}
	c.tokens--
	l.mu.Unlock()

	select {
	case c.sem <- struct{}{}:
	default:
		t := time.NewTimer(l.queueWait)
		defer t.Stop()
		select {
		case c.sem <- struct{}{}:
		case <-t.C:
			return nil, fmt.Errorf("%w: %d in flight for %q", ErrTooManyConcurrent, l.maxConcurrent, id)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for a slot: %w", ctx.Err())
		}
	}
	var once sync.Once
	return func() { once.Do(func() { <-c.sem }) }, nil
}

// InFlight reports calls in flight for id.
func (l *Limiter) InFlight(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.clients[id]; ok {
		return len(c.sem)
	}
	return 0
}
