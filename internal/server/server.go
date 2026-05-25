package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"github.com/splattner/burrow/internal/auth"
	"github.com/splattner/burrow/internal/config"
	"github.com/splattner/burrow/internal/kube"
	"github.com/splattner/burrow/internal/metrics"
	"github.com/splattner/burrow/internal/protocol"
	"github.com/splattner/burrow/internal/tunnel"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var errNoActiveClient = fmt.Errorf("no active client websocket session")
var errStreamOpenTimeout = fmt.Errorf("stream open timeout")

type Server struct {
	cfg     config.Config
	log     *logrus.Logger
	kube    *kube.Reconciler
	metrics *metrics.Registry
	auth    *auth.Verifier
	authErr error

	sessionsMu sync.RWMutex
	sessions   map[string]*session

	listenMu  sync.RWMutex
	listenURL string

	sessionSeq atomic.Uint64

	// runCtx is the context passed to Run; used for per-session background
	// goroutines (bridge listeners, heartbeat loops) that should live for the
	// server's lifetime.
	runCtx context.Context //nolint:containedctx

	startedCh chan struct{}
}

func New(cfg config.Config, logger *logrus.Logger) *Server {
	verifier, authErr := auth.NewVerifier(cfg)

	return &Server{
		cfg:       cfg,
		log:       logger,
		kube:      kube.NewReconcilerWithOptions(cfg.Namespace, cfg.EnableKubeAPI),
		metrics:   metrics.New(),
		auth:      verifier,
		authErr:   authErr,
		sessions:  make(map[string]*session),
		startedCh: make(chan struct{}),
	}
}

func (s *Server) Run(ctx context.Context) error {
	s.runCtx = ctx
	s.log.Infof("server mode: listen=%s namespace=%s", s.cfg.ServerAddr, s.cfg.Namespace)
	if err := s.kube.EnsureInfrastructure(ctx); err != nil {
		return fmt.Errorf("kube init: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/api/clients/", s.handleClientAPI)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", s.cfg.ServerAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() {
		_ = ln.Close()
	}()

	s.setListenAddr(ln.Addr().String())
	close(s.startedCh)

	errCh := make(chan error, 1)
	if s.cfg.TLSCertFile != "" {
		s.log.Infof("TLS enabled: cert=%s", s.cfg.TLSCertFile)
		go func() {
			if serveErr := httpServer.ServeTLS(ln, s.cfg.TLSCertFile, s.cfg.TLSKeyFile); serveErr != nil && serveErr != http.ErrServerClosed {
				errCh <- serveErr
			}
		}()
	} else {
		go func() {
			if serveErr := httpServer.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
				errCh <- serveErr
			}
		}()
	}

	go s.staleSweepLoop(ctx)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		s.closeAllSessions()
		return nil
	case serveErr := <-errCh:
		return fmt.Errorf("http serve: %w", serveErr)
	}
}

func (s *Server) WaitUntilStarted(timeout time.Duration) bool {
	if timeout <= 0 {
		<-s.startedCh
		return true
	}
	select {
	case <-s.startedCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

// WaitForClient blocks until the client with the given clientID has sent a
// successful register frame, or until timeout elapses.
func (s *Server) WaitForClient(clientID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.sessionsMu.RLock()
		sess := s.sessions[clientID]
		s.sessionsMu.RUnlock()

		if sess != nil {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return false
			}
			select {
			case <-sess.registeredCh:
				return true
			case <-time.After(remaining):
				return false
			}
		}

		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Server) WSURL() string {
	s.listenMu.RLock()
	defer s.listenMu.RUnlock()
	if s.listenURL == "" {
		return ""
	}
	if s.cfg.TLSCertFile != "" {
		return "wss://" + s.listenURL + "/ws"
	}
	return "ws://" + s.listenURL + "/ws"
}

// BridgeAddr returns the bridge listener address for the given client.
// Returns "" if no bridge listener has been started for that client yet.
func (s *Server) BridgeAddr(clientID string) string {
	s.sessionsMu.RLock()
	sess := s.sessions[clientID]
	s.sessionsMu.RUnlock()
	if sess == nil {
		return ""
	}
	return sess.getBridgeAddr()
}

// OpenStream opens a new multiplexed stream to the given client and waits for
// the client's open_ack before returning.
func (s *Server) OpenStream(clientID string, streamID uint64, target string) (*tunnel.Stream, error) {
	sess := s.lookupSession(clientID)
	if sess == nil {
		return nil, errNoActiveClient
	}

	openTimeout := s.cfg.HeartbeatInterval
	if openTimeout <= 0 {
		openTimeout = 5 * time.Second
	}

	stream, err := sess.openStream(streamID, target, openTimeout)
	if err != nil {
		return nil, err
	}
	s.metrics.IncStreams()
	return stream, nil
}

// SendData sends raw bytes over an existing stream to the given client.
func (s *Server) SendData(clientID string, streamID uint64, payload []byte) error {
	sess := s.lookupSession(clientID)
	if sess == nil {
		return errNoActiveClient
	}
	return sess.sendData(streamID, payload)
}

// CloseStream closes a stream to the given client.
func (s *Server) CloseStream(clientID string, streamID uint64) error {
	sess := s.lookupSession(clientID)
	if sess == nil {
		return nil
	}
	if sess.mux.Close(streamID) {
		s.metrics.DecStreams()
	}
	return sess.closeStream(streamID)
}

// ---------------------------------------------------------------------------
// WebSocket handling
// ---------------------------------------------------------------------------

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	authID, authErr := s.authenticateRequest(r)
	if authErr != nil {
		code := auth.ClassifyError(authErr)
		http.Error(w, fmt.Sprintf("unauthorized: code=%s reason=%s", code, authErr.Error()), http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Errorf("websocket upgrade failed: %v", err)
		return
	}

	send := make(chan []byte, 128)
	go s.writePump(conn, send)
	s.readPump(conn, send, authID)
}

func (s *Server) readPump(conn *websocket.Conn, send chan []byte, authID auth.Identity) {
	defer func() {
		_ = conn.Close()
	}()

	var activeSess *session

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || err == io.EOF {
				break
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				break
			}
			s.log.Errorf("server read error: %v", err)
			break
		}

		wf, err := protocol.DecodeWireFrame(payload)
		if err != nil {
			s.log.Errorf("server decode frame error: %v", err)
			continue
		}

		// Before registration: only process FrameRegister.
		if activeSess == nil {
			if wf.Kind != protocol.KindControl {
				continue
			}
			cf, decErr := protocol.DecodeControlFromWire(wf)
			if decErr != nil {
				s.log.Errorf("server decode control failed: %v", decErr)
				continue
			}
			if cf.Type != protocol.FrameRegister {
				continue
			}
			activeSess = s.handleRegister(conn, send, authID, cf)
			continue
		}

		s.handleWireFrame(activeSess, wf)
	}

	// Cleanup on connection drop.
	if activeSess != nil {
		if activeSess.onDisconnected(conn, send) {
			s.metrics.DecSessions()
			s.markClientDisconnected(activeSess.clientID)
			close(send)
		}
		// If onDisconnected returned false, prevSend was already closed by
		// swapConn during a concurrent reconnect.
	} else {
		// Never registered: stop the writePump.
		close(send)
	}
}

// handleRegister processes a FrameRegister control frame. It creates or finds
// the session for cf.ClientID, swaps in the new conn/send, starts per-session
// goroutines (bridge, heartbeat) on first registration, and sends register_ack.
// Returns the session to use for subsequent frames, or nil on rejection.
func (s *Server) handleRegister(conn *websocket.Conn, send chan []byte, authID auth.Identity, cf protocol.ControlFrame) *session {
	if !s.registerIdentityAllowed(authID, cf.ClientID) {
		s.log.Warnf("register rejected: client_id=%q does not match authenticated identity", cf.ClientID)
		if wire, encErr := protocol.EncodeControlFrame(0, protocol.ControlFrame{
			Type:    protocol.FrameError,
			Message: "client identity mismatch",
		}); encErr == nil {
			_ = conn.WriteMessage(websocket.BinaryMessage, wire)
		}
		close(send)
		_ = conn.Close()
		return nil
	}

	// Compute the session ID for this connection up front so the counter is
	// incremented exactly once regardless of whether the session is new or a
	// reconnect.
	newSessionID := fmt.Sprintf("sess-%d", s.sessionSeq.Add(1))

	// Look up or create the session.
	s.sessionsMu.Lock()
	sess, exists := s.sessions[cf.ClientID]
	if !exists {
		sess = newSession(cf.ClientID, authID, newSessionID)
		s.sessions[cf.ClientID] = sess
	}
	s.sessionsMu.Unlock()

	prevConn, prevSend := sess.swapConn(conn, send, authID, newSessionID)
	sess.target = cf.Target

	if prevConn == nil {
		// First registration for this client.
		s.metrics.IncSessions()
		s.log.Infof("client connected: client_id=%q target=%q session=%s", cf.ClientID, cf.Target, sess.sessionID)
	} else {
		s.log.Infof("client reconnected: client_id=%q target=%q session=%s", cf.ClientID, cf.Target, sess.sessionID)
	}

	// Close the old connection to unblock the previous readPump goroutine.
	if prevSend != nil {
		close(prevSend)
	}
	if prevConn != nil {
		_ = prevConn.Close()
	}

	sess.hb.Beat(time.Now())

	// Start per-session bridge listener (once per session lifetime).
	sess.bridgeOnce.Do(func() {
		s.startBridgeForSession(sess)
	})

	// Reconcile the Kubernetes Service for this client.
	bridgePort := sess.bridgePort()
	if _, err := s.kube.EnsureClientService(s.runCtx, cf.ClientID, cf.Target, bridgePort); err != nil {
		s.log.Errorf("reconcile client service failed client=%q: %v", cf.ClientID, err)
	}

	// Signal that this client has registered (idempotent on reconnect).
	sess.registeredOnce.Do(func() {
		close(sess.registeredCh)
	})

	// Start heartbeat loop (once per session lifetime, runs for server lifetime).
	if !exists {
		go s.runHeartbeatLoop(s.runCtx, sess)
	}

	ack, err := protocol.EncodeControlFrame(0, protocol.ControlFrame{
		Type:              protocol.FrameRegisterAck,
		SessionID:         sess.sessionID,
		PreviousSessionID: cf.PreviousSessionID,
	})
	if err == nil {
		_ = sess.enqueue(ack)
	}

	return sess
}

func (s *Server) writePump(conn *websocket.Conn, send <-chan []byte) {
	defer func() {
		_ = conn.Close()
	}()

	for msg := range send {
		if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			s.log.Errorf("server write error: %v", err)
			return
		}
	}
}

func (s *Server) handleWireFrame(sess *session, wf protocol.WireFrame) {
	switch wf.Kind {
	case protocol.KindData:
		if err := sess.mux.Deliver(wf.StreamID, wf.Payload); err != nil {
			if errors.Is(err, tunnel.ErrStreamBackpressure) {
				s.metrics.IncStreamBackpressureDrops()
				s.log.Warnf("server stream=%d backpressure; closing stream", wf.StreamID)
				if sess.mux.Close(wf.StreamID) {
					s.metrics.DecStreams()
				}
				_ = sess.closeStream(wf.StreamID)
				return
			}
			s.log.Errorf("server deliver stream=%d failed: %v", wf.StreamID, err)
		}
	case protocol.KindControl:
		cf, err := protocol.DecodeControlFromWire(wf)
		if err != nil {
			s.log.Errorf("server decode control failed: %v", err)
			return
		}
		s.handleControl(sess, cf)
	}
}

func (s *Server) handleControl(sess *session, cf protocol.ControlFrame) {
	switch cf.Type {
	case protocol.FrameOpenAck:
		sess.resolvePendingOpen(cf.StreamID, nil)
	case protocol.FrameClose:
		if sess.mux.Close(cf.StreamID) {
			s.metrics.DecStreams()
		}
		sess.resolvePendingOpen(cf.StreamID, fmt.Errorf("stream closed before open ack"))
	case protocol.FrameError:
		sess.resolvePendingOpen(cf.StreamID, fmt.Errorf("client error: %s", cf.Message))
	case protocol.FrameHeartbeat:
		sess.hb.Beat(time.Now())
		ack, err := protocol.EncodeControlFrame(0, protocol.ControlFrame{Type: protocol.FrameHeartbeatAck})
		if err == nil {
			_ = sess.enqueue(ack)
		}
	case protocol.FrameHeartbeatAck:
		sess.hb.Beat(time.Now())
	case protocol.FrameRegister:
		// Re-register on an active session is a no-op.
	default:
	}
}

// ---------------------------------------------------------------------------
// Bridge listener
// ---------------------------------------------------------------------------

func (s *Server) startBridgeForSession(sess *session) {
	bindAddr := bridgeBindAddr(s.cfg.BridgeHost)
	if bindAddr == "" {
		return
	}

	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		s.log.Errorf("bridge listener start failed for client %q: %v", sess.clientID, err)
		return
	}

	sess.bridgeMu.Lock()
	sess.bridgeLn = ln
	sess.bridgeAddr = ln.Addr().String()
	sess.bridgeMu.Unlock()

	go s.runBridgeListenerForSession(s.runCtx, ln, sess)
}

func (s *Server) runBridgeListenerForSession(ctx context.Context, ln net.Listener, sess *session) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			s.log.Errorf("bridge accept error client=%q: %v", sess.clientID, err)
			return
		}

		go s.handleBridgeConn(sess, conn)
	}
}

func (s *Server) handleBridgeConn(sess *session, conn net.Conn) {
	defer func() {
		_ = conn.Close()
	}()

	streamID := sess.streamSeq.Add(1)

	openTimeout := s.cfg.HeartbeatInterval
	if openTimeout <= 0 {
		openTimeout = 5 * time.Second
	}

	stream, err := sess.openStream(streamID, sess.target, openTimeout)
	if err != nil {
		s.log.Errorf("bridge open stream=%d client=%q failed: %v", streamID, sess.clientID, err)
		return
	}
	s.metrics.IncStreams()

	tunnelToPodDone := make(chan struct{})
	go func() {
		defer close(tunnelToPodDone)
		for payload := range stream.In {
			if _, writeErr := conn.Write(payload); writeErr != nil {
				return
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, readErr := conn.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			if sendErr := sess.sendData(streamID, payload); sendErr != nil {
				s.log.Errorf("bridge send stream=%d failed: %v", streamID, sendErr)
				break
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				s.log.Errorf("bridge read stream=%d failed: %v", streamID, readErr)
			}
			break
		}
	}

	if sess.mux.Close(streamID) {
		s.metrics.DecStreams()
	}
	_ = sess.closeStream(streamID)
	_ = conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
	select {
	case <-tunnelToPodDone:
	case <-time.After(200 * time.Millisecond):
	}
}

// ---------------------------------------------------------------------------
// Heartbeat
// ---------------------------------------------------------------------------

func (s *Server) runHeartbeatLoop(ctx context.Context, sess *session) {
	interval := s.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	timeout := s.cfg.HeartbeatTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !sess.hasActive() {
				continue
			}

			if sess.hb.TimedOut(time.Now(), timeout) {
				s.log.Warnf("session client=%q heartbeat timed out; closing connection", sess.clientID)
				sess.mu.RLock()
				conn := sess.conn
				sess.mu.RUnlock()
				if conn != nil {
					_ = conn.Close()
				}
				continue
			}

			frame, err := protocol.EncodeControlFrame(0, protocol.ControlFrame{Type: protocol.FrameHeartbeat})
			if err != nil {
				continue
			}
			_ = sess.enqueue(frame)
		}
	}
}

// ---------------------------------------------------------------------------
// Stale sweep
// ---------------------------------------------------------------------------

func (s *Server) staleSweepLoop(ctx context.Context) {
	interval := s.cfg.SweepInterval
	if interval <= 0 {
		interval = 1 * time.Minute
	}
	staleAge := s.cfg.StaleServiceAge
	if staleAge <= 0 {
		staleAge = 10 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted := s.kube.SweepStaleServices(ctx, staleAge, time.Now())
			if len(deleted) > 0 {
				s.metrics.AddStaleServicesDeleted(int64(len(deleted)))
				s.log.Infof("stale service sweep removed clients=%v", deleted)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		"# HELP burrow_sessions_active Number of active websocket client sessions.\n"+
			"# TYPE burrow_sessions_active gauge\n"+
			"burrow_sessions_active %d\n"+
			"# HELP burrow_streams_active Number of active multiplexed streams.\n"+
			"# TYPE burrow_streams_active gauge\n"+
			"burrow_streams_active %d\n"+
			"# HELP burrow_stale_services_deleted_total Total stale client services deleted by sweeps.\n"+
			"# TYPE burrow_stale_services_deleted_total counter\n"+
			"burrow_stale_services_deleted_total %d\n"+
			"# HELP burrow_stream_backpressure_drops_total Total stream drops caused by backpressure saturation.\n"+
			"# TYPE burrow_stream_backpressure_drops_total counter\n"+
			"burrow_stream_backpressure_drops_total %d\n",
		s.metrics.Sessions(),
		s.metrics.Streams(),
		s.metrics.StaleServicesDeleted(),
		s.metrics.StreamBackpressureDrops(),
	)
}

// handleClientAPI serves /api/clients/{clientID}/bridge-addr.
// Returns the bridge listener address for a connected client as plain text,
// or 404 if the client is not connected or has no bridge listener yet.
func (s *Server) handleClientAPI(w http.ResponseWriter, r *http.Request) {
	// Expect path: /api/clients/{clientID}/bridge-addr
	path := strings.TrimPrefix(r.URL.Path, "/api/clients/")
	clientID, suffix, ok := strings.Cut(path, "/")
	if !ok || suffix != "bridge-addr" || clientID == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	s.sessionsMu.RLock()
	sess := s.sessions[clientID]
	s.sessionsMu.RUnlock()

	if sess == nil {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}
	addr := sess.getBridgeAddr()
	if addr == "" {
		http.Error(w, "no bridge listener", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprint(w, addr)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Server) lookupSession(clientID string) *session {
	s.sessionsMu.RLock()
	sess := s.sessions[clientID]
	s.sessionsMu.RUnlock()
	if sess == nil || !sess.hasActive() {
		return nil
	}
	return sess
}

func (s *Server) setListenAddr(addr string) {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	s.listenURL = addr
}

func (s *Server) authenticateRequest(r *http.Request) (auth.Identity, error) {
	if s.authErr != nil {
		s.log.Errorf("authorization rejected: auth setup error: %v", s.authErr)
		return auth.Identity{}, s.authErr
	}
	if s.auth == nil {
		return auth.Identity{Method: "none"}, nil
	}
	authID, err := s.auth.Authenticate(r)
	if err != nil {
		s.log.Warnf("authorization rejected: %v", err)
		return auth.Identity{}, err
	}
	return authID, nil
}

func (s *Server) registerIdentityAllowed(authID auth.Identity, clientID string) bool {
	if authID.Method != "jwt" {
		return true
	}
	if authID.Subject == "" {
		return false
	}
	return authID.Subject == clientID
}

func (s *Server) markClientDisconnected(clientID string) {
	if clientID == "" {
		return
	}
	if err := s.kube.MarkClientDisconnected(s.runCtx, clientID); err != nil {
		s.log.Errorf("mark client disconnected failed client=%q: %v", clientID, err)
	}
}

func (s *Server) closeAllSessions() {
	s.sessionsMu.RLock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessionsMu.RUnlock()

	for _, sess := range sessions {
		sess.forceClose()
	}
}

// bridgeBindAddr returns host:0 from the configured bridge host so each
// per-client listener gets a unique random port. Returns "" if disabled.
func bridgeBindAddr(cfgBridgeHost string) string {
	h := strings.TrimSpace(cfgBridgeHost)
	if h == "" {
		return ""
	}
	// Accept both "host" and "host:port" (legacy) — always bind on port 0.
	if strings.Contains(h, ":") {
		host, _, err := net.SplitHostPort(h)
		if err == nil {
			return host + ":0"
		}
	}
	return h + ":0"
}

func safeSend(ch chan []byte, payload []byte) (ok bool, full bool) {
	defer func() {
		if recover() != nil {
			ok = false
			full = false
		}
	}()

	select {
	case ch <- payload:
		return true, false
	default:
		return true, true
	}
}
