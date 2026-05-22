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
	"github.com/splattner/k8s-reverse-tunnel/internal/auth"
	"github.com/splattner/k8s-reverse-tunnel/internal/config"
	"github.com/splattner/k8s-reverse-tunnel/internal/kube"
	"github.com/splattner/k8s-reverse-tunnel/internal/metrics"
	"github.com/splattner/k8s-reverse-tunnel/internal/protocol"
	"github.com/splattner/k8s-reverse-tunnel/internal/tunnel"
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
	mux     *tunnel.Multiplexer
	kube    *kube.Reconciler
	metrics *metrics.Registry
	hb      *tunnel.HeartbeatTracker
	auth    *auth.Verifier
	authErr error

	connMu sync.RWMutex
	conn   *websocket.Conn
	send   chan []byte
	alive  bool
	authID auth.Identity

	pendingMu    sync.Mutex
	pendingOpens map[uint64]chan error

	clientMu       sync.RWMutex
	activeClientID string
	activeTarget   string

	listenMu  sync.RWMutex
	listenURL string
	bridgeURL string

	streamSeq  atomic.Uint64
	sessionSeq atomic.Uint64

	sessionIDMu sync.RWMutex
	sessionID   string

	registeredOnce sync.Once
	registeredCh   chan struct{}
	startedCh      chan struct{}
}

func New(cfg config.Config, logger *logrus.Logger) *Server {
	verifier, authErr := auth.NewVerifier(cfg)

	return &Server{
		cfg:          cfg,
		log:          logger,
		mux:          tunnel.NewMultiplexer(),
		kube:         kube.NewReconcilerWithBridgeOptions(cfg.Namespace, cfg.BridgeAddr, cfg.EnableKubeAPI),
		metrics:      metrics.New(),
		hb:           tunnel.NewHeartbeatTracker(time.Now()),
		auth:         verifier,
		authErr:      authErr,
		pendingOpens: make(map[uint64]chan error),
		registeredCh: make(chan struct{}),
		startedCh:    make(chan struct{}),
	}
}

func (s *Server) Run(ctx context.Context) error {
	s.log.Infof("server mode: listen=%s namespace=%s", s.cfg.ServerAddr, s.cfg.Namespace)
	if err := s.kube.EnsureInfrastructure(ctx); err != nil {
		return fmt.Errorf("kube init: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	httpServer := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", s.cfg.ServerAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() {
		_ = ln.Close()
	}()

	var bridgeLn net.Listener
	if s.cfg.BridgeAddr != "" {
		bridgeLn, err = net.Listen("tcp", s.cfg.BridgeAddr)
		if err != nil {
			return fmt.Errorf("bridge listen: %w", err)
		}
		defer func() {
			_ = bridgeLn.Close()
		}()
		s.setBridgeAddr(bridgeLn.Addr().String())
	}

	s.setListenAddr(ln.Addr().String())
	close(s.startedCh)

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpServer.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	if bridgeLn != nil {
		go s.runBridgeListener(ctx, bridgeLn)
	}

	go s.heartbeatLoop(ctx)
	go s.staleSweepLoop(ctx)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		s.closeSession()
		s.closeAllStreamsWithMetrics()
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

func (s *Server) WaitForClient(timeout time.Duration) bool {
	if timeout <= 0 {
		<-s.registeredCh
		return true
	}

	select {
	case <-s.registeredCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *Server) WSURL() string {
	s.listenMu.RLock()
	defer s.listenMu.RUnlock()
	if s.listenURL == "" {
		return ""
	}
	return "ws://" + s.listenURL + "/ws"
}

func (s *Server) BridgeAddr() string {
	s.listenMu.RLock()
	defer s.listenMu.RUnlock()
	return s.bridgeURL
}

func (s *Server) OpenStream(streamID uint64, target string) (*tunnel.Stream, error) {
	if !s.hasActiveSession() {
		return nil, errNoActiveClient
	}

	stream, err := s.mux.Open(streamID)
	if err != nil {
		return nil, err
	}

	ackCh := s.trackPendingOpen(streamID)
	defer s.untrackPendingOpen(streamID)

	wire, err := protocol.EncodeControlFrame(streamID, protocol.ControlFrame{
		Type:     protocol.FrameOpen,
		StreamID: streamID,
		Target:   target,
	})
	if err != nil {
		s.mux.Close(streamID)
		return nil, fmt.Errorf("encode open control: %w", err)
	}

	if err := s.enqueue(wire); err != nil {
		s.mux.Close(streamID)
		return nil, err
	}

	openTimeout := s.cfg.HeartbeatInterval
	if openTimeout <= 0 {
		openTimeout = 5 * time.Second
	}

	select {
	case ackErr := <-ackCh:
		if ackErr != nil {
			s.mux.Close(streamID)
			return nil, ackErr
		}
		s.metrics.IncStreams()
		return stream, nil
	case <-time.After(openTimeout):
		s.mux.Close(streamID)
		return nil, errStreamOpenTimeout
	}
}

func (s *Server) SendData(streamID uint64, payload []byte) error {
	wire, err := protocol.EncodeDataFrame(streamID, payload)
	if err != nil {
		return fmt.Errorf("encode data frame: %w", err)
	}
	return s.enqueue(wire)
}

func (s *Server) CloseStream(streamID uint64) error {
	if closed := s.mux.Close(streamID); closed {
		s.metrics.DecStreams()
	}
	wire, err := protocol.EncodeControlFrame(streamID, protocol.ControlFrame{
		Type:     protocol.FrameClose,
		StreamID: streamID,
	})
	if err != nil {
		return fmt.Errorf("encode close control: %w", err)
	}
	return s.enqueue(wire)
}

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

	prevConn, prevSend, send := s.swapSession(conn, authID)
	if prevConn != nil {
		s.onSessionReplaced()
		_ = prevConn.Close()
	}
	if prevSend != nil {
		close(prevSend)
	}

	go s.writePump(conn, send)
	s.readPump(conn)
	s.closeSessionIfCurrent(conn, send)
}

func (s *Server) readPump(conn *websocket.Conn) {
	defer func() {
		_ = conn.Close()
	}()

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || err == io.EOF {
				return
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			s.log.Errorf("server read error: %v", err)
			return
		}

		wf, err := protocol.DecodeWireFrame(payload)
		if err != nil {
			s.log.Errorf("server decode frame error: %v", err)
			continue
		}

		s.handleWireFrame(wf)
	}
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

func (s *Server) handleWireFrame(wf protocol.WireFrame) {
	switch wf.Kind {
	case protocol.KindData:
		if err := s.mux.Deliver(wf.StreamID, wf.Payload); err != nil {
			if errors.Is(err, tunnel.ErrStreamBackpressure) {
				s.metrics.IncStreamBackpressureDrops()
				s.log.Warnf("server stream=%d backpressure; closing stream", wf.StreamID)
				_ = s.CloseStream(wf.StreamID)
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
		s.handleControl(cf)
	}
}

func (s *Server) handleControl(cf protocol.ControlFrame) {
	switch cf.Type {
	case protocol.FrameRegister:
		if !s.registerIdentityAllowed(cf.ClientID) {
			s.log.Warnf("register rejected: client_id=%q does not match authenticated identity", cf.ClientID)
			_ = s.sendControlError(0, "client identity mismatch")
			s.closeSession()
			return
		}
		s.hb.Beat(time.Now())
		s.setActiveClient(cf.ClientID, cf.Target)
		if _, err := s.kube.EnsureClientService(context.Background(), cf.ClientID, cf.Target); err != nil {
			s.log.Errorf("reconcile client service failed client=%q: %v", cf.ClientID, err)
		}
		s.registeredOnce.Do(func() {
			close(s.registeredCh)
		})
		ack, err := protocol.EncodeControlFrame(0, protocol.ControlFrame{
			Type:              protocol.FrameRegisterAck,
			SessionID:         s.currentSessionID(),
			PreviousSessionID: cf.PreviousSessionID,
		})
		if err == nil {
			_ = s.enqueue(ack)
		}
	case protocol.FrameOpenAck:
		s.resolvePendingOpen(cf.StreamID, nil)
	case protocol.FrameClose:
		if closed := s.mux.Close(cf.StreamID); closed {
			s.metrics.DecStreams()
		}
		s.resolvePendingOpen(cf.StreamID, fmt.Errorf("stream closed before open ack"))
	case protocol.FrameError:
		s.resolvePendingOpen(cf.StreamID, fmt.Errorf("client error: %s", cf.Message))
	case protocol.FrameHeartbeat:
		s.hb.Beat(time.Now())
		ack, err := protocol.EncodeControlFrame(0, protocol.ControlFrame{Type: protocol.FrameHeartbeatAck})
		if err == nil {
			_ = s.enqueue(ack)
		}
	case protocol.FrameHeartbeatAck:
		s.hb.Beat(time.Now())
	default:
	}
}

func (s *Server) enqueue(payload []byte) error {
	s.connMu.RLock()
	send := s.send
	if send == nil {
		s.connMu.RUnlock()
		return errNoActiveClient
	}
	if !s.alive {
		s.connMu.RUnlock()
		return errNoActiveClient
	}

	ok, full := trySend(send, payload)
	s.connMu.RUnlock()
	if !ok {
		return errNoActiveClient
	}
	if full {
		return fmt.Errorf("server send queue full")
	}
	return nil
}

func (s *Server) swapSession(conn *websocket.Conn, authID auth.Identity) (*websocket.Conn, chan []byte, chan []byte) {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	prevConn := s.conn
	prevSend := s.send

	s.conn = conn
	s.send = make(chan []byte, 128)
	s.alive = true
	s.authID = authID
	s.setCurrentSessionID(fmt.Sprintf("sess-%d", s.sessionSeq.Add(1)))
	if prevConn == nil {
		s.metrics.IncSessions()
	}
	s.hb.Beat(time.Now())
	return prevConn, prevSend, s.send
}

func (s *Server) closeSession() {
	s.connMu.Lock()
	conn := s.conn
	send := s.send
	hadActive := conn != nil
	s.conn = nil
	s.send = nil
	s.alive = false
	s.authID = auth.Identity{}
	s.setCurrentSessionID("")
	s.connMu.Unlock()
	s.onSessionDropped(fmt.Errorf("session closed"))
	if hadActive {
		s.metrics.DecSessions()
	}

	if send != nil {
		close(send)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *Server) closeSessionIfCurrent(conn *websocket.Conn, send chan []byte) {
	s.connMu.Lock()
	if s.conn != conn || s.send != send {
		s.connMu.Unlock()
		return
	}
	s.conn = nil
	s.send = nil
	s.alive = false
	s.authID = auth.Identity{}
	s.setCurrentSessionID("")
	s.connMu.Unlock()
	s.onSessionDropped(fmt.Errorf("session closed"))
	s.metrics.DecSessions()

	if send != nil {
		close(send)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *Server) runBridgeListener(ctx context.Context, ln net.Listener) {
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
			s.log.Errorf("bridge accept error: %v", err)
			return
		}

		go s.handleBridgeConn(conn)
	}
}

func (s *Server) handleBridgeConn(conn net.Conn) {
	defer func() {
		_ = conn.Close()
	}()

	streamID := s.streamSeq.Add(1)
	stream, err := s.OpenStream(streamID, s.activeClientTarget())
	if err != nil {
		s.log.Errorf("bridge open stream=%d failed: %v", streamID, err)
		return
	}

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
			if sendErr := s.SendData(streamID, payload); sendErr != nil {
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

	_ = s.CloseStream(streamID)
	_ = conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
	select {
	case <-tunnelToPodDone:
	case <-time.After(200 * time.Millisecond):
	}
}

func (s *Server) hasActiveSession() bool {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	return s.send != nil && s.alive
}

func (s *Server) trackPendingOpen(streamID uint64) chan error {
	ch := make(chan error, 1)
	s.pendingMu.Lock()
	s.pendingOpens[streamID] = ch
	s.pendingMu.Unlock()
	return ch
}

func (s *Server) untrackPendingOpen(streamID uint64) {
	s.pendingMu.Lock()
	delete(s.pendingOpens, streamID)
	s.pendingMu.Unlock()
}

func (s *Server) resolvePendingOpen(streamID uint64, err error) {
	s.pendingMu.Lock()
	ch, ok := s.pendingOpens[streamID]
	s.pendingMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

func (s *Server) heartbeatLoop(ctx context.Context) {
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
			if !s.hasActiveSession() {
				continue
			}

			if s.hb.TimedOut(time.Now(), timeout) {
				s.log.Warn("server session heartbeat timed out; closing active websocket")
				s.closeSession()
				continue
			}

			frame, err := protocol.EncodeControlFrame(0, protocol.ControlFrame{Type: protocol.FrameHeartbeat})
			if err != nil {
				continue
			}
			_ = s.enqueue(frame)
		}
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		"# HELP krt_sessions_active Number of active websocket client sessions.\n"+
			"# TYPE krt_sessions_active gauge\n"+
			"krt_sessions_active %d\n"+
			"# HELP krt_streams_active Number of active multiplexed streams.\n"+
			"# TYPE krt_streams_active gauge\n"+
			"krt_streams_active %d\n"+
			"# HELP krt_stale_services_deleted_total Total stale client services deleted by sweeps.\n"+
			"# TYPE krt_stale_services_deleted_total counter\n"+
			"krt_stale_services_deleted_total %d\n"+
			"# HELP krt_stream_backpressure_drops_total Total stream drops caused by backpressure saturation.\n"+
			"# TYPE krt_stream_backpressure_drops_total counter\n"+
			"krt_stream_backpressure_drops_total %d\n",
		s.metrics.Sessions(),
		s.metrics.Streams(),
		s.metrics.StaleServicesDeleted(),
		s.metrics.StreamBackpressureDrops(),
	)
}

func (s *Server) closeAllStreamsWithMetrics() {
	active := s.mux.Count()
	if active <= 0 {
		return
	}
	s.mux.CloseAll()
	for i := 0; i < active; i++ {
		s.metrics.DecStreams()
	}
}

func (s *Server) onSessionDropped(err error) {
	s.closeAllStreamsWithMetrics()
	s.failPendingOpens(err)
	s.markActiveClientDisconnected()
}

func (s *Server) onSessionReplaced() {
	s.closeAllStreamsWithMetrics()
	s.failPendingOpens(fmt.Errorf("session replaced"))
	s.markActiveClientDisconnected()
}

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

func (s *Server) failPendingOpens(err error) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for _, ch := range s.pendingOpens {
		select {
		case ch <- err:
		default:
		}
	}
}

func trySend(ch chan []byte, payload []byte) (ok bool, full bool) {
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

func (s *Server) setListenAddr(addr string) {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	s.listenURL = addr
}

func (s *Server) setBridgeAddr(addr string) {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	s.bridgeURL = addr
}

func (s *Server) currentSessionID() string {
	s.sessionIDMu.RLock()
	defer s.sessionIDMu.RUnlock()
	return s.sessionID
}

func (s *Server) setCurrentSessionID(id string) {
	s.sessionIDMu.Lock()
	defer s.sessionIDMu.Unlock()
	s.sessionID = id
}

func (s *Server) setActiveClient(clientID, target string) {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	s.activeClientID = clientID
	s.activeTarget = target
}

func (s *Server) activeClientTarget() string {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return s.activeTarget
}

func (s *Server) markActiveClientDisconnected() {
	s.clientMu.Lock()
	clientID := s.activeClientID
	s.activeClientID = ""
	s.activeTarget = ""
	s.clientMu.Unlock()

	if clientID == "" {
		return
	}
	if err := s.kube.MarkClientDisconnected(context.Background(), clientID); err != nil {
		s.log.Errorf("mark client disconnected failed client=%q: %v", clientID, err)
	}
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

func (s *Server) registerIdentityAllowed(clientID string) bool {
	s.connMu.RLock()
	authID := s.authID
	s.connMu.RUnlock()

	if authID.Method != "jwt" {
		return true
	}
	if authID.Subject == "" {
		return false
	}
	return authID.Subject == clientID
}

func (s *Server) sendControlError(streamID uint64, message string) error {
	frame, err := protocol.EncodeControlFrame(streamID, protocol.ControlFrame{
		Type:     protocol.FrameError,
		StreamID: streamID,
		Message:  message,
	})
	if err != nil {
		return err
	}
	return s.enqueue(frame)
}
