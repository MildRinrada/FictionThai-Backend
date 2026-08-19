package ratelimit

import (
	"context"
	"sync"
	"time"
)

// memoryWindow is one fixed window's counter for one key.
type memoryWindow struct {
	count     int64
	expiresAt time.Time
}

// MemoryLimiter is a per-process fixed-window limiter.
//
// It is the fallback when Redis is not configured. Because the counters live in
// process memory, N API instances allow N times the limit - acceptable for
// single-instance development, which is why the server logs a warning when this
// limiter is selected outside development.
type MemoryLimiter struct {
	mu      sync.Mutex
	windows map[string]*memoryWindow

	stop chan struct{}
	once sync.Once
}

// NewMemoryLimiter returns a limiter that sweeps expired windows periodically
// so that a long-running process does not accumulate keys forever.
func NewMemoryLimiter() *MemoryLimiter {
	l := &MemoryLimiter{
		windows: make(map[string]*memoryWindow),
		stop:    make(chan struct{}),
	}
	go l.sweepLoop(time.Minute)
	return l
}

func (l *MemoryLimiter) Allow(_ context.Context, key string, policy Policy) Result {
	now := time.Now()
	windowKey := policy.Name + ":" + key

	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.windows[windowKey]
	if !ok || now.After(w.expiresAt) {
		w = &memoryWindow{expiresAt: now.Add(policy.Window)}
		l.windows[windowKey] = w
	}

	w.count++
	if w.count > int64(policy.Limit) {
		return denied(policy, time.Until(w.expiresAt))
	}
	return allowed(policy, w.count)
}

// Peek reads the current window without counting a hit.
func (l *MemoryLimiter) Peek(_ context.Context, key string, policy Policy) Result {
	now := time.Now()
	windowKey := policy.Name + ":" + key

	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.windows[windowKey]
	if !ok || now.After(w.expiresAt) {
		return allowed(policy, 0)
	}
	if w.count >= int64(policy.Limit) {
		return denied(policy, time.Until(w.expiresAt))
	}
	return allowed(policy, w.count)
}

func (l *MemoryLimiter) Close() error {
	l.once.Do(func() { close(l.stop) })
	return nil
}

func (l *MemoryLimiter) sweepLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stop:
			return
		case now := <-ticker.C:
			l.mu.Lock()
			for key, w := range l.windows {
				if now.After(w.expiresAt) {
					delete(l.windows, key)
				}
			}
			l.mu.Unlock()
		}
	}
}
