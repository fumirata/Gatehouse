package monitor

import (
	"sync"
	"time"
)

// Clock abstracts time so the state machine can be tested without waiting for
// real backoff intervals, which would otherwise make the test suite take
// minutes.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock is the production Clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FakeClock is a manually advanced Clock for tests.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	due time.Time
	ch  chan time.Time
}

// NewFakeClock returns a FakeClock starting at the given instant.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After registers a waiter, giving tests a way to know precisely when the
// monitor has parked. Advance is what releases it.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan time.Time, 1)
	w := &fakeWaiter{due: c.now.Add(d), ch: ch}
	c.waiters = append(c.waiters, w)

	// A zero or negative delay must not hang: fire immediately.
	if !w.due.After(c.now) {
		ch <- c.now
	}
	return ch
}

// Advance moves the clock forward, releasing every waiter that comes due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)

	var keep []*fakeWaiter
	var fire []*fakeWaiter
	for _, w := range c.waiters {
		if !w.due.After(c.now) {
			fire = append(fire, w)
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	now := c.now
	c.mu.Unlock()

	for _, w := range fire {
		w.ch <- now
	}
}

// Set moves the clock to an absolute instant without releasing any waiter.
//
// Tests use it to place the clock at a precise offset from a recorded event,
// which is more reliable than accumulating relative advances.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// WaiterCount reports how many timers are currently outstanding. Tests use it
// to wait until the monitor is genuinely parked before advancing the clock,
// which is what keeps fake-clock tests deterministic instead of flaky.
func (c *FakeClock) WaiterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// WaitForWaiter blocks until at least n timers are outstanding, or the deadline
// passes.
func (c *FakeClock) WaitForWaiter(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.WaiterCount() >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
