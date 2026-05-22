package tunnel

import (
	"sync"
	"time"
)

type HeartbeatTracker struct {
	mu   sync.RWMutex
	last time.Time
}

func NewHeartbeatTracker(now time.Time) *HeartbeatTracker {
	return &HeartbeatTracker{last: now}
}

func (h *HeartbeatTracker) Beat(at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = at
}

func (h *HeartbeatTracker) LastBeat() time.Time {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.last
}

func (h *HeartbeatTracker) TimedOut(now time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	h.mu.RLock()
	last := h.last
	h.mu.RUnlock()
	return now.Sub(last) > timeout
}
