package client

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebastian/k8s-reverse-tunnel/internal/config"
	"github.com/sebastian/k8s-reverse-tunnel/internal/logging"
)

func TestOpenLocalConnIsIdempotentPerStreamID(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test target: %v", err)
	}
	defer ln.Close()

	var accepted atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(100 * time.Millisecond))
				buf := make([]byte, 1)
				_, _ = c.Read(buf)
			}(conn)
		}
	}()

	c := New(config.Config{}, logging.NoOp())
	if err := c.openLocalConn(42, ln.Addr().String()); err != nil {
		t.Fatalf("first openLocalConn failed: %v", err)
	}
	if err := c.openLocalConn(42, ln.Addr().String()); err != nil {
		t.Fatalf("second openLocalConn failed: %v", err)
	}

	time.Sleep(120 * time.Millisecond)
	if got := accepted.Load(); got != 1 {
		t.Fatalf("expected a single local dial/accept, got %d", got)
	}

	c.closeAllLocalConns()
	_ = ln.Close()
	<-done
}
