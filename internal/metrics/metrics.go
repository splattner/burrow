package metrics

import "sync/atomic"

type Registry struct {
	sessions                atomic.Int64
	streams                 atomic.Int64
	staleServicesDeleted    atomic.Int64
	streamBackpressureDrops atomic.Int64
}

func New() *Registry {
	return &Registry{}
}

func (r *Registry) IncSessions() {
	r.sessions.Add(1)
}

func (r *Registry) DecSessions() {
	r.sessions.Add(-1)
}

func (r *Registry) IncStreams() {
	r.streams.Add(1)
}

func (r *Registry) DecStreams() {
	r.streams.Add(-1)
}

func (r *Registry) AddStaleServicesDeleted(n int64) {
	if n <= 0 {
		return
	}
	r.staleServicesDeleted.Add(n)
}

func (r *Registry) IncStreamBackpressureDrops() {
	r.streamBackpressureDrops.Add(1)
}

func (r *Registry) Sessions() int64 {
	return r.sessions.Load()
}

func (r *Registry) Streams() int64 {
	return r.streams.Load()
}

func (r *Registry) StaleServicesDeleted() int64 {
	return r.staleServicesDeleted.Load()
}

func (r *Registry) StreamBackpressureDrops() int64 {
	return r.streamBackpressureDrops.Load()
}
