package server

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/splattner/burrow/internal/auth"
	"github.com/splattner/burrow/internal/protocol"
	"github.com/splattner/burrow/internal/tunnel"
)

// session holds the per-client WebSocket connection and all associated state.
// One session exists per unique clientID. A session persists across reconnects:
// only the conn/send fields are swapped on reconnect; the bridge listener and
// registration channel remain stable.
type session struct {
	clientID  string
	authID    auth.Identity
	sessionID string
	target    string

	mu    sync.RWMutex
	conn  *websocket.Conn
	send  chan []byte
	alive bool

	mux *tunnel.Multiplexer
	hb  *tunnel.HeartbeatTracker

	pendingMu    sync.Mutex
	pendingOpens map[uint64]chan error

	streamSeq atomic.Uint64

	// bridge listener — started once when the client first registers
	bridgeOnce sync.Once
	bridgeMu   sync.RWMutex
	bridgeLn   net.Listener
	bridgeAddr string

	// registeredCh is closed once the client has successfully registered.
	// It stays closed across reconnects.
	registeredOnce sync.Once
	registeredCh   chan struct{}
}

func newSession(clientID string, authID auth.Identity, sessionID string) *session {
	return &session{
		clientID:     clientID,
		authID:       authID,
		sessionID:    sessionID,
		mux:          tunnel.NewMultiplexer(),
		hb:           tunnel.NewHeartbeatTracker(time.Now()),
		pendingOpens: make(map[uint64]chan error),
		registeredCh: make(chan struct{}),
	}
}

// swapConn atomically installs conn and send as the active connection for this
// session. The caller must close prevSend (to stop the old writePump) and
// prevConn (to terminate the old readPump) after this returns.
func (sess *session) swapConn(conn *websocket.Conn, send chan []byte, authID auth.Identity, sessionID string) (prevConn *websocket.Conn, prevSend chan []byte) {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	prevConn = sess.conn
	prevSend = sess.send

	sess.conn = conn
	sess.send = send
	sess.alive = true
	sess.authID = authID
	sess.sessionID = sessionID
	sess.hb.Beat(time.Now())

	return prevConn, prevSend
}

func (sess *session) hasActive() bool {
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	return sess.send != nil && sess.alive
}

func (sess *session) enqueue(payload []byte) error {
	sess.mu.RLock()
	send := sess.send
	alive := sess.alive
	sess.mu.RUnlock()

	if send == nil || !alive {
		return errNoActiveClient
	}

	ok, full := trySend(send, payload)
	if !ok {
		return errNoActiveClient
	}
	if full {
		return fmt.Errorf("server send queue full")
	}
	return nil
}

func (sess *session) openStream(streamID uint64, target string, openTimeout time.Duration) (*tunnel.Stream, error) {
	if !sess.hasActive() {
		return nil, errNoActiveClient
	}

	stream, err := sess.mux.Open(streamID)
	if err != nil {
		return nil, err
	}

	ackCh := sess.trackPendingOpen(streamID)
	defer sess.untrackPendingOpen(streamID)

	wire, err := protocol.EncodeControlFrame(streamID, protocol.ControlFrame{
		Type:     protocol.FrameOpen,
		StreamID: streamID,
		Target:   target,
	})
	if err != nil {
		sess.mux.Close(streamID)
		return nil, fmt.Errorf("encode open control: %w", err)
	}

	if err := sess.enqueue(wire); err != nil {
		sess.mux.Close(streamID)
		return nil, err
	}

	if openTimeout <= 0 {
		openTimeout = 5 * time.Second
	}

	select {
	case ackErr := <-ackCh:
		if ackErr != nil {
			sess.mux.Close(streamID)
			return nil, ackErr
		}
		return stream, nil
	case <-time.After(openTimeout):
		sess.mux.Close(streamID)
		return nil, errStreamOpenTimeout
	}
}

func (sess *session) sendData(streamID uint64, payload []byte) error {
	wire, err := protocol.EncodeDataFrame(streamID, payload)
	if err != nil {
		return fmt.Errorf("encode data frame: %w", err)
	}
	return sess.enqueue(wire)
}

func (sess *session) closeStream(streamID uint64) error {
	sess.mux.Close(streamID)
	wire, err := protocol.EncodeControlFrame(streamID, protocol.ControlFrame{
		Type:     protocol.FrameClose,
		StreamID: streamID,
	})
	if err != nil {
		return fmt.Errorf("encode close control: %w", err)
	}
	return sess.enqueue(wire)
}

func (sess *session) trackPendingOpen(streamID uint64) chan error {
	ch := make(chan error, 1)
	sess.pendingMu.Lock()
	sess.pendingOpens[streamID] = ch
	sess.pendingMu.Unlock()
	return ch
}

func (sess *session) untrackPendingOpen(streamID uint64) {
	sess.pendingMu.Lock()
	delete(sess.pendingOpens, streamID)
	sess.pendingMu.Unlock()
}

func (sess *session) resolvePendingOpen(streamID uint64, err error) {
	sess.pendingMu.Lock()
	ch, ok := sess.pendingOpens[streamID]
	sess.pendingMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

func (sess *session) failPendingOpens(err error) {
	sess.pendingMu.Lock()
	defer sess.pendingMu.Unlock()
	for _, ch := range sess.pendingOpens {
		select {
		case ch <- err:
		default:
		}
	}
}

func (sess *session) closeAllStreams() int {
	active := sess.mux.Count()
	sess.mux.CloseAll()
	return active
}

// onDisconnected is called when the WebSocket connection drops. It clears the
// conn fields, closes all open streams, and fails pending open requests.
// Returns true if the conn was still current when this was called (i.e. we
// actually cleaned something up).
func (sess *session) onDisconnected(conn *websocket.Conn, send chan []byte) bool {
	sess.mu.Lock()
	if sess.conn != conn || sess.send != send {
		sess.mu.Unlock()
		return false
	}
	sess.conn = nil
	sess.send = nil
	sess.alive = false
	sess.mu.Unlock()

	sess.closeAllStreams()
	sess.failPendingOpens(fmt.Errorf("session disconnected"))
	return true
}

// forceClose unconditionally clears the session conn regardless of which
// connection is current. Used when a new client evicts a prior session.
func (sess *session) forceClose() {
	sess.mu.Lock()
	conn := sess.conn
	send := sess.send
	sess.conn = nil
	sess.send = nil
	sess.alive = false
	sess.mu.Unlock()

	sess.closeAllStreams()
	sess.failPendingOpens(fmt.Errorf("session replaced"))

	if send != nil {
		close(send)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (sess *session) sendControlError(streamID uint64, message string) error {
	frame, err := protocol.EncodeControlFrame(streamID, protocol.ControlFrame{
		Type:     protocol.FrameError,
		StreamID: streamID,
		Message:  message,
	})
	if err != nil {
		return err
	}
	return sess.enqueue(frame)
}

func (sess *session) getBridgeAddr() string {
	sess.bridgeMu.RLock()
	defer sess.bridgeMu.RUnlock()
	return sess.bridgeAddr
}

// bridgePort returns the TCP port of the per-client bridge listener, or 0 if
// no listener has been started yet.
func (sess *session) bridgePort() int32 {
	sess.bridgeMu.RLock()
	defer sess.bridgeMu.RUnlock()
	if sess.bridgeLn == nil {
		return 0
	}
	addr := sess.bridgeLn.Addr()
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return int32(tcp.Port)
	}
	return 0
}
