package server

import (
	"testing"
	"time"

	"github.com/splattner/burrow/internal/auth"
	"github.com/splattner/burrow/internal/config"
	"github.com/splattner/burrow/internal/logging"
	"github.com/splattner/burrow/internal/protocol"
	"github.com/splattner/burrow/internal/tunnel"
)

func TestHandleWireFrameBackpressureClosesStreamAndCountsDrop(t *testing.T) {
	s := New(config.Config{Namespace: "default"}, logging.NoOp())

	sess := newSession("test-client", auth.Identity{}, "sess-test")
	sess.mux = tunnel.NewMultiplexerWithConfig(tunnel.MultiplexerConfig{
		StreamBufferSize: 1,
		DeliveryTimeout:  20 * time.Millisecond,
	})
	s.sessionsMu.Lock()
	s.sessions["test-client"] = sess
	s.sessionsMu.Unlock()

	stream, err := sess.mux.Open(77)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := sess.mux.Deliver(77, []byte("first")); err != nil {
		t.Fatalf("prime stream queue: %v", err)
	}

	s.metrics.IncStreams()

	s.handleWireFrame(sess, protocol.WireFrame{
		Kind:     protocol.KindData,
		StreamID: 77,
		Payload:  []byte("second"),
	})

	if got := s.metrics.StreamBackpressureDrops(); got != 1 {
		t.Fatalf("expected one backpressure drop, got %d", got)
	}
	if got := s.metrics.Streams(); got != 0 {
		t.Fatalf("expected stream metric to decrement after close, got %d", got)
	}
	if _, ok := <-stream.In; !ok {
		return
	}
	if _, ok := <-stream.In; ok {
		t.Fatal("expected stream channel to close after backpressure-triggered close")
	}
}
