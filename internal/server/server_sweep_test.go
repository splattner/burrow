package server

import (
	"context"
	"testing"
	"time"

	"github.com/splattner/k8s-reverse-tunnel/internal/config"
	"github.com/splattner/k8s-reverse-tunnel/internal/logging"
)

func TestStaleSweepLoopRemovesDisconnectedClient(t *testing.T) {
	s := New(config.Config{
		Namespace:       "default",
		SweepInterval:   10 * time.Millisecond,
		StaleServiceAge: 20 * time.Millisecond,
	}, logging.NoOp())

	ctx := context.Background()
	if _, err := s.kube.EnsureClientService(ctx, "client-sweep", "127.0.0.1:5432"); err != nil {
		t.Fatalf("ensure client service: %v", err)
	}
	if err := s.kube.MarkClientDisconnected(ctx, "client-sweep"); err != nil {
		t.Fatalf("mark client disconnected: %v", err)
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.staleSweepLoop(loopCtx)
	}()

	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		if _, ok := s.kube.GetServiceForClient("client-sweep"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for stale sweep to delete disconnected client service")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.metrics.StaleServicesDeleted(); got < 1 {
		t.Fatalf("expected stale deletion metric to increment, got %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stale sweep loop did not stop after context cancellation")
	}
}

func TestStaleSweepLoopKeepsRecentlyDisconnectedClient(t *testing.T) {
	s := New(config.Config{
		Namespace:       "default",
		SweepInterval:   10 * time.Millisecond,
		StaleServiceAge: 2 * time.Second,
	}, logging.NoOp())

	ctx := context.Background()
	if _, err := s.kube.EnsureClientService(ctx, "client-fresh", "127.0.0.1:6000"); err != nil {
		t.Fatalf("ensure client service: %v", err)
	}
	if err := s.kube.MarkClientDisconnected(ctx, "client-fresh"); err != nil {
		t.Fatalf("mark client disconnected: %v", err)
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.staleSweepLoop(loopCtx)
	}()

	// Wait across multiple sweep intervals but below stale age threshold.
	time.Sleep(120 * time.Millisecond)

	if _, ok := s.kube.GetServiceForClient("client-fresh"); !ok {
		cancel()
		<-done
		t.Fatal("service was deleted before stale age threshold")
	}
	if got := s.metrics.StaleServicesDeleted(); got != 0 {
		cancel()
		<-done
		t.Fatalf("expected stale deletion metric to remain 0, got %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stale sweep loop did not stop after context cancellation")
	}
}
