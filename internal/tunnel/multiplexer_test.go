package tunnel

import (
	"errors"
	"testing"
	"time"
)

func TestMultiplexerOpenDeliverCloseLifecycle(t *testing.T) {
	m := NewMultiplexer()

	s, err := m.Open(7)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("expected 1 stream, got %d", m.Count())
	}

	if err := m.Deliver(7, []byte("payload")); err != nil {
		t.Fatalf("deliver payload: %v", err)
	}

	select {
	case got := <-s.In:
		if string(got) != "payload" {
			t.Fatalf("unexpected payload: %q", string(got))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for payload")
	}

	if closed := m.Close(7); !closed {
		t.Fatal("expected close to return true")
	}
	if m.Count() != 0 {
		t.Fatalf("expected 0 streams after close, got %d", m.Count())
	}

	if err := m.Deliver(7, []byte("x")); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("expected ErrStreamNotFound, got %v", err)
	}
}

func TestMultiplexerRejectsDuplicateOpen(t *testing.T) {
	m := NewMultiplexer()
	if _, err := m.Open(1); err != nil {
		t.Fatalf("first open failed: %v", err)
	}
	if _, err := m.Open(1); !errors.Is(err, ErrStreamExists) {
		t.Fatalf("expected ErrStreamExists, got %v", err)
	}
}

func TestHeartbeatTrackerTimeout(t *testing.T) {
	base := time.Unix(1700000000, 0)
	h := NewHeartbeatTracker(base)

	if h.TimedOut(base.Add(5*time.Second), 10*time.Second) {
		t.Fatal("heartbeat should not be timed out yet")
	}
	if !h.TimedOut(base.Add(11*time.Second), 10*time.Second) {
		t.Fatal("heartbeat should be timed out")
	}

	h.Beat(base.Add(12 * time.Second))
	if h.TimedOut(base.Add(20*time.Second), 10*time.Second) {
		t.Fatal("heartbeat should have been refreshed")
	}
}

func TestMultiplexerDeliverReturnsBackpressureWhenQueueFull(t *testing.T) {
	m := NewMultiplexerWithConfig(MultiplexerConfig{
		StreamBufferSize: 1,
		DeliveryTimeout:  20 * time.Millisecond,
	})

	_, err := m.Open(99)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	if err := m.Deliver(99, []byte("first")); err != nil {
		t.Fatalf("first deliver should succeed: %v", err)
	}

	start := time.Now()
	err = m.Deliver(99, []byte("second"))
	if !errors.Is(err, ErrStreamBackpressure) {
		t.Fatalf("expected ErrStreamBackpressure, got %v", err)
	}
	if time.Since(start) < 15*time.Millisecond {
		t.Fatal("expected deliver to wait for delivery timeout before backpressure error")
	}
}
