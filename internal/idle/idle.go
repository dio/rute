// Package idle tracks activity so a server started on demand can exit after a
// period with no calls and no attached clients.
package idle

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Tracker counts active work and remembers when the last work ended. It is
// safe for concurrent use.
type Tracker struct {
	now func() time.Time

	mu     sync.Mutex
	active int
	last   time.Time

	stopOnce sync.Once
	stopping chan struct{}
}

// New returns a Tracker whose idle period starts now.
func New() *Tracker {
	return &Tracker{now: time.Now, last: time.Now(), stopping: make(chan struct{})}
}

// Begin records the start of active work and returns the function that ends
// it.
func (tracker *Tracker) Begin() (end func()) {
	tracker.mu.Lock()
	tracker.active++
	tracker.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			tracker.mu.Lock()
			tracker.active--
			tracker.last = tracker.now()
			tracker.mu.Unlock()
		})
	}
}

// Middleware counts each request to next as active work.
func (tracker *Tracker) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		end := tracker.Begin()
		defer end()
		next.ServeHTTP(writer, request)
	})
}

// IdleFor returns how long the tracker has had no active work, or zero while
// work is active.
func (tracker *Tracker) IdleFor() time.Duration {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.active > 0 {
		return 0
	}
	return tracker.now().Sub(tracker.last)
}

// Active returns the amount of active work.
func (tracker *Tracker) Active() int {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.active
}

// Stop closes the channel returned by Stopping, which long-lived work such as
// attached clients watches so that a server shutdown does not wait for it.
func (tracker *Tracker) Stop() {
	tracker.stopOnce.Do(func() { close(tracker.stopping) })
}

// Stopping is closed when Stop is called.
func (tracker *Tracker) Stopping() <-chan struct{} {
	return tracker.stopping
}

// Wait returns when the tracker has been idle for timeout, checking every
// interval, or when ctx is done. It reports whether the tracker became idle.
func (tracker *Tracker) Wait(ctx context.Context, timeout, interval time.Duration) bool {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if tracker.IdleFor() >= timeout {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
