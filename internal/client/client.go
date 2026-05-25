package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"

	"github.com/splattner/burrow/internal/bridge"
	"github.com/splattner/burrow/internal/config"
	"github.com/splattner/burrow/internal/protocol"
	"github.com/splattner/burrow/internal/tunnel"
)

type Client struct {
	cfg    config.Config
	log    *logrus.Logger
	mux    *tunnel.Multiplexer
	bridge *bridge.TCPBridge

	sendMu sync.Mutex
	sendCh chan []byte

	connMu sync.Mutex
	conns  map[uint64]net.Conn

	sessionGeneration atomic.Uint64

	sessionMu     sync.RWMutex
	lastSessionID string
}

var errAuthRejected = errors.New("authentication rejected by server")
var errTokenExpired = errors.New("bearer token expired")
var errTokenNotYetValid = errors.New("bearer token not yet valid")
var errTokenRefreshRequired = errors.New("token refresh required")

func New(cfg config.Config, logger *logrus.Logger) *Client {
	return &Client{
		cfg:    cfg,
		log:    logger,
		mux:    tunnel.NewMultiplexer(),
		bridge: bridge.NewTCPBridge(),
		conns:  make(map[uint64]net.Conn),
	}
}

func (c *Client) Run(ctx context.Context) error {
	c.log.Infof("client mode: server=%s client_id=%s target=%s", c.cfg.ServerURL, c.cfg.ClientID, c.cfg.LocalTarget)
	if c.cfg.TLSSkipVerify {
		c.log.Warn("TLS certificate verification disabled (--tls-skip-verify): do not use in production")
	}
	if c.cfg.ServerURL == "" {
		return fmt.Errorf("server URL is required (--server-url / BURROW_SERVER_URL)")
	}
	if c.cfg.LocalTarget == "" {
		return fmt.Errorf("local target is required (--local-target / BURROW_LOCAL_TARGET)")
	}

	failures := 0
	for {
		err := c.runSession(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}

		delay := c.nextRetryDelay(err, failures)
		failures++
		c.log.Warnf("client session ended: %v (retrying in %s)", err, delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

func (c *Client) runSession(ctx context.Context) error {
	c.sessionGeneration.Add(1)
	prevSessionID := c.getLastSessionID()
	bearer, err := c.resolveBearerToken()
	if err != nil {
		return err
	}

	refreshAt, hasRefreshAt, refreshErr := tokenRefreshDeadline(bearer, c.cfg.TokenRefreshWindow)
	if refreshErr != nil {
		c.log.Debugf("client token refresh check skipped: %v", refreshErr)
	}
	if hasRefreshAt && !time.Now().Before(refreshAt) {
		return fmt.Errorf("%w: refresh deadline reached at %s", errTokenRefreshRequired, refreshAt.Format(time.RFC3339))
	}

	headers := http.Header{}
	if bearer != "" {
		headers.Set("Authorization", "Bearer "+bearer)
	}

	conn, resp, err := c.buildDialer().DialContext(ctx, c.cfg.ServerURL, headers)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return classifyUnauthorizedResponse(resp)
		}
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	send := make(chan []byte, 128)
	c.setSendChan(send)
	defer c.clearSendChan(send)

	register, err := protocol.EncodeControlFrame(0, protocol.ControlFrame{
		Type:              protocol.FrameRegister,
		ClientID:          c.cfg.ClientID,
		Target:            c.cfg.LocalTarget,
		PreviousSessionID: prevSessionID,
	})
	if err != nil {
		return fmt.Errorf("encode register frame: %w", err)
	}
	send <- register

	errCh := make(chan error, 2)
	go c.writePump(conn, send, errCh)
	go c.readPump(conn, errCh)

	var refreshCh <-chan time.Time
	if hasRefreshAt {
		delay := time.Until(refreshAt)
		if delay <= 0 {
			delay = time.Millisecond
		}
		refreshCh = time.After(delay)
	}

	select {
	case <-ctx.Done():
		c.closeAllLocalConns()
		close(send)
		return nil
	case pumpErr := <-errCh:
		c.closeAllLocalConns()
		close(send)
		return pumpErr
	case <-refreshCh:
		c.closeAllLocalConns()
		close(send)
		_ = conn.Close()
		return fmt.Errorf("%w: reached refresh window before token expiry", errTokenRefreshRequired)
	}
}

// buildDialer returns a websocket.Dialer configured for the current client
// settings. It handles two optional behaviours, which may be combined:
//
//   - ConnectAddr: connect to a specific IP/host:port at the TCP level while
//     keeping cfg.ServerURL for TLS SNI and the HTTP Host header.
//   - TLSSkipVerify: disable TLS certificate verification (self-signed certs,
//     expired certs, etc.). Logs a warning when active.
func (c *Client) buildDialer() *websocket.Dialer {
	if c.cfg.ConnectAddr == "" && !c.cfg.TLSSkipVerify {
		return websocket.DefaultDialer
	}

	dialer := &websocket.Dialer{
		Proxy:            websocket.DefaultDialer.Proxy,
		HandshakeTimeout: websocket.DefaultDialer.HandshakeTimeout,
	}

	if c.cfg.TLSSkipVerify {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	if c.cfg.ConnectAddr != "" {
		// If ConnectAddr has no port, borrow the port from the server URL.
		connectAddr := c.cfg.ConnectAddr
		if _, _, err := net.SplitHostPort(connectAddr); err != nil {
			u, parseErr := url.Parse(c.cfg.ServerURL)
			port := ""
			if parseErr == nil {
				port = u.Port()
			}
			if port == "" {
				switch {
				case strings.HasPrefix(c.cfg.ServerURL, "wss://"), strings.HasPrefix(c.cfg.ServerURL, "https://"):
					port = "443"
				default:
					port = "80"
				}
			}
			connectAddr = net.JoinHostPort(connectAddr, port)
		}
		c.log.Infof("connect-addr override: TCP connection will go to %s", connectAddr)
		addr := connectAddr
		dialer.NetDialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	}

	return dialer
}

func (c *Client) resolveBearerToken() (string, error) {
	if path := strings.TrimSpace(c.cfg.BearerTokenFile); path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read bearer token file %q: %w", path, err)
		}
		token := strings.TrimSpace(string(content))
		if token == "" {
			return "", fmt.Errorf("bearer token file %q is empty", path)
		}
		return token, nil
	}

	token := strings.TrimSpace(c.cfg.BearerToken)
	if token == "" {
		return "", fmt.Errorf("missing bearer token: set BURROW_BEARER_TOKEN or BURROW_BEARER_TOKEN_FILE")
	}
	return token, nil
}

func tokenRefreshDeadline(rawToken string, refreshWindow time.Duration) (time.Time, bool, error) {
	if refreshWindow <= 0 {
		refreshWindow = 30 * time.Second
	}

	claims := &jwt.RegisteredClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(rawToken, claims); err != nil {
		return time.Time{}, false, fmt.Errorf("parse token claims: %w", err)
	}
	if claims.ExpiresAt == nil {
		return time.Time{}, false, nil
	}
	return claims.ExpiresAt.Add(-refreshWindow), true, nil
}

func classifyUnauthorizedResponse(resp *http.Response) error {
	defer func() {
		_ = resp.Body.Close()
	}()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}

	switch {
	case strings.Contains(msg, "code=token_expired"):
		return fmt.Errorf("%w: %s", errTokenExpired, msg)
	case strings.Contains(msg, "code=token_not_yet_valid"):
		return fmt.Errorf("%w: %s", errTokenNotYetValid, msg)
	default:
		return fmt.Errorf("%w: %s", errAuthRejected, msg)
	}
}

func (c *Client) nextRetryDelay(err error, failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}

	base := c.cfg.RetryInterval
	if base <= 0 {
		base = time.Second
	}
	max := 15 * time.Second

	if errors.Is(err, errAuthRejected) {
		base = c.cfg.AuthRetryInterval
		if base <= 0 {
			base = 5 * time.Second
		}
		max = 60 * time.Second
	}

	if errors.Is(err, errTokenExpired) || errors.Is(err, errTokenRefreshRequired) {
		if strings.TrimSpace(c.cfg.BearerTokenFile) == "" {
			base = 15 * time.Second
			max = 2 * time.Minute
		} else {
			base = time.Second
			max = 10 * time.Second
		}
	}

	if errors.Is(err, errTokenNotYetValid) {
		base = 2 * time.Second
		max = 30 * time.Second
	}

	delay := base
	for i := 0; i < failures && delay < max; i++ {
		delay *= 2
		if delay > max {
			delay = max
			break
		}
	}
	return delay
}

func (c *Client) readPump(conn *websocket.Conn, errCh chan<- error) {
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || err == io.EOF {
				errCh <- nil
				return
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				errCh <- nil
				return
			}
			errCh <- fmt.Errorf("client read: %w", err)
			return
		}

		wf, err := protocol.DecodeWireFrame(payload)
		if err != nil {
			c.log.Errorf("client decode frame failed: %v", err)
			continue
		}
		c.handleWireFrame(wf)
	}
}

func (c *Client) writePump(conn *websocket.Conn, send <-chan []byte, errCh chan<- error) {
	for msg := range send {
		if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			errCh <- fmt.Errorf("client write: %w", err)
			return
		}
	}
	errCh <- nil
}

func (c *Client) handleWireFrame(wf protocol.WireFrame) {
	switch wf.Kind {
	case protocol.KindControl:
		cf, err := protocol.DecodeControlFromWire(wf)
		if err != nil {
			c.log.Errorf("client decode control failed: %v", err)
			return
		}
		c.handleControl(cf)
	case protocol.KindData:
		c.handleData(wf.StreamID, wf.Payload)
	}
}

func (c *Client) handleControl(cf protocol.ControlFrame) {
	switch cf.Type {
	case protocol.FrameOpen:
		target := cf.Target
		if target == "" {
			target = c.cfg.LocalTarget
		}
		if err := c.openLocalConn(cf.StreamID, target); err != nil {
			c.log.Errorf("open local conn failed stream=%d: %v", cf.StreamID, err)
			_ = c.sendControl(protocol.ControlFrame{
				Type:     protocol.FrameError,
				StreamID: cf.StreamID,
				Message:  err.Error(),
			})
			return
		}
		_ = c.sendControl(protocol.ControlFrame{Type: protocol.FrameOpenAck, StreamID: cf.StreamID})
	case protocol.FrameClose:
		c.closeLocalConn(cf.StreamID)
	case protocol.FrameRegisterAck:
		c.setLastSessionID(cf.SessionID)
		c.log.Infof("connected to server: server=%s client_id=%s target=%s session=%s",
			c.cfg.ServerURL, c.cfg.ClientID, c.cfg.LocalTarget, cf.SessionID)
	case protocol.FrameHeartbeat:
		_ = c.sendControl(protocol.ControlFrame{Type: protocol.FrameHeartbeatAck})
	case protocol.FrameHeartbeatAck:
		// Server acknowledged client heartbeat (reserved for future client timeout tracking).
	default:
	}
}

func (c *Client) handleData(streamID uint64, payload []byte) {
	conn, ok := c.getLocalConn(streamID)
	if !ok {
		c.log.Warnf("dropping data for unknown stream %d", streamID)
		return
	}

	if _, err := conn.Write(payload); err != nil {
		c.log.Errorf("write to local conn stream=%d failed: %v", streamID, err)
		c.closeLocalConn(streamID)
		_ = c.sendControl(protocol.ControlFrame{Type: protocol.FrameClose, StreamID: streamID})
	}
}

func (c *Client) openLocalConn(streamID uint64, target string) error {
	if _, exists := c.getLocalConn(streamID); exists {
		return nil
	}

	conn, err := net.Dial("tcp", target)
	if err != nil {
		return fmt.Errorf("dial local target %s: %w", target, err)
	}

	c.connMu.Lock()
	if _, exists := c.conns[streamID]; exists {
		c.connMu.Unlock()
		_ = conn.Close()
		return nil
	}
	c.conns[streamID] = conn
	c.connMu.Unlock()

	go c.pumpLocalToTunnel(streamID, conn)
	return nil
}

func (c *Client) pumpLocalToTunnel(streamID uint64, conn net.Conn) {
	generation := c.sessionGeneration.Load()
	buf := make([]byte, 32*1024)
	for {
		if !c.isCurrentSessionGeneration(generation) {
			c.closeLocalConn(streamID)
			return
		}

		n, err := conn.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			if !c.isCurrentSessionGeneration(generation) {
				c.closeLocalConn(streamID)
				return
			}
			if sendErr := c.sendData(streamID, payload); sendErr != nil {
				c.log.Errorf("send tunnel data stream=%d failed: %v", streamID, sendErr)
				c.closeLocalConn(streamID)
				return
			}
		}

		if err != nil {
			if err != io.EOF {
				c.log.Errorf("read local conn stream=%d failed: %v", streamID, err)
			}
			_ = c.sendControl(protocol.ControlFrame{Type: protocol.FrameClose, StreamID: streamID})
			c.closeLocalConn(streamID)
			return
		}
	}
}

func (c *Client) sendControl(frame protocol.ControlFrame) error {
	wire, err := protocol.EncodeControlFrame(frame.StreamID, frame)
	if err != nil {
		return err
	}
	return c.enqueue(wire)
}

func (c *Client) sendData(streamID uint64, payload []byte) error {
	wire, err := protocol.EncodeDataFrame(streamID, payload)
	if err != nil {
		return err
	}
	return c.enqueue(wire)
}

func (c *Client) enqueue(payload []byte) error {
	c.sendMu.Lock()
	send := c.sendCh
	c.sendMu.Unlock()
	if send == nil {
		return fmt.Errorf("no active websocket writer")
	}

	ok, full := safeSend(send, payload)
	if !ok {
		return fmt.Errorf("no active websocket writer")
	}
	if full {
		return fmt.Errorf("client send queue full")
	}
	return nil
}

func (c *Client) setSendChan(send chan []byte) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.sendCh = send
}

func (c *Client) clearSendChan(send chan []byte) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.sendCh == send {
		c.sendCh = nil
	}
}

func (c *Client) getLocalConn(streamID uint64) (net.Conn, bool) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	conn, ok := c.conns[streamID]
	return conn, ok
}

func (c *Client) closeLocalConn(streamID uint64) {
	c.connMu.Lock()
	conn, ok := c.conns[streamID]
	if ok {
		delete(c.conns, streamID)
	}
	c.connMu.Unlock()

	if ok {
		_ = conn.Close()
	}
}

func (c *Client) closeAllLocalConns() {
	c.connMu.Lock()
	conns := c.conns
	c.conns = make(map[uint64]net.Conn)
	c.connMu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (c *Client) isCurrentSessionGeneration(generation uint64) bool {
	return c.sessionGeneration.Load() == generation
}

func (c *Client) getLastSessionID() string {
	c.sessionMu.RLock()
	defer c.sessionMu.RUnlock()
	return c.lastSessionID
}

func (c *Client) setLastSessionID(id string) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.lastSessionID = id
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
