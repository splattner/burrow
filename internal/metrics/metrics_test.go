package metrics

import "testing"

func TestRegistryCounters(t *testing.T) {
	r := New()

	r.IncSessions()
	r.IncStreams()
	r.IncStreams()
	r.DecStreams()
	r.AddStaleServicesDeleted(2)
	r.AddStaleServicesDeleted(0)
	r.AddStaleServicesDeleted(-5)
	r.IncStreamBackpressureDrops()
	r.IncStreamBackpressureDrops()

	if got := r.Sessions(); got != 1 {
		t.Fatalf("expected sessions=1, got %d", got)
	}
	if got := r.Streams(); got != 1 {
		t.Fatalf("expected streams=1, got %d", got)
	}
	if got := r.StaleServicesDeleted(); got != 2 {
		t.Fatalf("expected staleServicesDeleted=2, got %d", got)
	}
	if got := r.StreamBackpressureDrops(); got != 2 {
		t.Fatalf("expected streamBackpressureDrops=2, got %d", got)
	}
}
