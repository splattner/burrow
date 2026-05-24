package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/splattner/burrow/internal/config"
	"github.com/splattner/burrow/internal/logging"
	"github.com/splattner/burrow/internal/protocol"
)

// ---- helpers ---------------------------------------------------------------

// makeExpiredJWT signs a HS256 JWT with the given expiry (zero = no exp claim).
func makeExpiredJWT(t *testing.T, expiry time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{}
	if !expiry.IsZero() {
		claims["exp"] = expiry.Unix()
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-secret"))
	require.NoError(t, err)
	return tok
}

// fakeUnauthorized returns an *http.Response with the given body text.
func fakeUnauthorized(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// localConnPair returns a connected TCP pair over loopback.
func localConnPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	ch := make(chan net.Conn, 1)
	go func() {
		conn, _ := ln.Accept()
		ch <- conn
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	server = <-ch
	return server, client
}

// unusedAddr starts a listener, records its address, then closes it —
// returning an address that will yield "connection refused".
func unusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// clientWithCh creates a Client with an active buffered send channel.
func clientWithCh(ch chan []byte) *Client {
	c := New(config.Config{}, logging.NoOp())
	c.setSendChan(ch)
	return c
}

// decodeControlMsg decodes the first message from ch as a ControlFrame.
func decodeControlMsg(t *testing.T, ch <-chan []byte) protocol.ControlFrame {
	t.Helper()
	select {
	case msg := <-ch:
		wf, err := protocol.DecodeWireFrame(msg)
		require.NoError(t, err)
		cf, err := protocol.DecodeControlFromWire(wf)
		require.NoError(t, err)
		return cf
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message in send channel")
		panic("unreachable")
	}
}

// ---- resolveBearerToken ----------------------------------------------------

func TestResolveBearerToken_InlineToken(t *testing.T) {
	c := New(config.Config{BearerToken: "my-token"}, logging.NoOp())
	tok, err := c.resolveBearerToken()
	require.NoError(t, err)
	assert.Equal(t, "my-token", tok)
}

func TestResolveBearerToken_TrimsWhitespace(t *testing.T) {
	c := New(config.Config{BearerToken: "  my-token\n"}, logging.NoOp())
	tok, err := c.resolveBearerToken()
	require.NoError(t, err)
	assert.Equal(t, "my-token", tok)
}

func TestResolveBearerToken_EmptyToken(t *testing.T) {
	c := New(config.Config{}, logging.NoOp())
	_, err := c.resolveBearerToken()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing bearer token")
}

func TestResolveBearerToken_FromFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "token-*.txt")
	require.NoError(t, err)
	_, _ = f.WriteString("file-token\n")
	_ = f.Close()

	c := New(config.Config{BearerTokenFile: f.Name()}, logging.NoOp())
	tok, err := c.resolveBearerToken()
	require.NoError(t, err)
	assert.Equal(t, "file-token", tok)
}

func TestResolveBearerToken_FileNotFound(t *testing.T) {
	c := New(config.Config{BearerTokenFile: "/nonexistent/path.jwt"}, logging.NoOp())
	_, err := c.resolveBearerToken()
	require.Error(t, err)
}

func TestResolveBearerToken_EmptyFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "token-*.txt")
	require.NoError(t, err)
	_ = f.Close()

	c := New(config.Config{BearerTokenFile: f.Name()}, logging.NoOp())
	_, err = c.resolveBearerToken()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// File takes precedence over inline token when both are set.
func TestResolveBearerToken_FilePreferredOverInline(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "token-*.txt")
	require.NoError(t, err)
	_, _ = f.WriteString("file-token")
	_ = f.Close()

	c := New(config.Config{BearerToken: "inline-token", BearerTokenFile: f.Name()}, logging.NoOp())
	tok, err := c.resolveBearerToken()
	require.NoError(t, err)
	assert.Equal(t, "file-token", tok)
}

// ---- tokenRefreshDeadline --------------------------------------------------

func TestTokenRefreshDeadline_WithExpiry(t *testing.T) {
	expiry := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	tok := makeExpiredJWT(t, expiry)

	deadline, has, err := tokenRefreshDeadline(tok, 30*time.Second)
	require.NoError(t, err)
	assert.True(t, has)
	assert.WithinDuration(t, expiry.Add(-30*time.Second), deadline, time.Second)
}

func TestTokenRefreshDeadline_NoExpiry(t *testing.T) {
	tok := makeExpiredJWT(t, time.Time{})

	_, has, err := tokenRefreshDeadline(tok, 30*time.Second)
	require.NoError(t, err)
	assert.False(t, has)
}

func TestTokenRefreshDeadline_DefaultsRefreshWindowTo30s(t *testing.T) {
	expiry := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	tok := makeExpiredJWT(t, expiry)

	deadline, has, err := tokenRefreshDeadline(tok, 0)
	require.NoError(t, err)
	assert.True(t, has)
	// default window is 30s
	assert.WithinDuration(t, expiry.Add(-30*time.Second), deadline, time.Second)
}

func TestTokenRefreshDeadline_InvalidToken(t *testing.T) {
	_, _, err := tokenRefreshDeadline("not-a-jwt", 30*time.Second)
	require.Error(t, err)
}

// ---- classifyUnauthorizedResponse ------------------------------------------

func TestClassifyUnauthorizedResponse_TokenExpired(t *testing.T) {
	err := classifyUnauthorizedResponse(fakeUnauthorized("auth error: code=token_expired"))
	assert.True(t, errors.Is(err, errTokenExpired))
}

func TestClassifyUnauthorizedResponse_TokenNotYetValid(t *testing.T) {
	err := classifyUnauthorizedResponse(fakeUnauthorized("auth error: code=token_not_yet_valid"))
	assert.True(t, errors.Is(err, errTokenNotYetValid))
}

func TestClassifyUnauthorizedResponse_InvalidToken(t *testing.T) {
	err := classifyUnauthorizedResponse(fakeUnauthorized("auth error: code=invalid_token"))
	assert.True(t, errors.Is(err, errAuthRejected))
}

func TestClassifyUnauthorizedResponse_EmptyBody_UsesStatusText(t *testing.T) {
	err := classifyUnauthorizedResponse(fakeUnauthorized(""))
	assert.True(t, errors.Is(err, errAuthRejected))
	assert.Contains(t, err.Error(), "Unauthorized")
}

// ---- nextRetryDelay --------------------------------------------------------

func TestNextRetryDelay_GenericError_ZeroFailures(t *testing.T) {
	c := New(config.Config{RetryInterval: time.Second}, logging.NoOp())
	assert.Equal(t, time.Second, c.nextRetryDelay(fmt.Errorf("err"), 0))
}

func TestNextRetryDelay_GenericError_ExponentialBackoff(t *testing.T) {
	c := New(config.Config{RetryInterval: time.Second}, logging.NoOp())
	assert.Equal(t, time.Second, c.nextRetryDelay(fmt.Errorf("err"), 0))
	assert.Equal(t, 2*time.Second, c.nextRetryDelay(fmt.Errorf("err"), 1))
	assert.Equal(t, 4*time.Second, c.nextRetryDelay(fmt.Errorf("err"), 2))
	assert.Equal(t, 8*time.Second, c.nextRetryDelay(fmt.Errorf("err"), 3))
	assert.Equal(t, 15*time.Second, c.nextRetryDelay(fmt.Errorf("err"), 4)) // capped
	assert.Equal(t, 15*time.Second, c.nextRetryDelay(fmt.Errorf("err"), 10))
}

func TestNextRetryDelay_GenericError_DefaultsBase(t *testing.T) {
	// Zero RetryInterval defaults to 1s.
	c := New(config.Config{}, logging.NoOp())
	assert.Equal(t, time.Second, c.nextRetryDelay(fmt.Errorf("err"), 0))
}

func TestNextRetryDelay_AuthRejected_UsesAuthInterval(t *testing.T) {
	c := New(config.Config{AuthRetryInterval: 5 * time.Second}, logging.NoOp())
	delay := c.nextRetryDelay(fmt.Errorf("%w", errAuthRejected), 0)
	assert.Equal(t, 5*time.Second, delay)
}

func TestNextRetryDelay_AuthRejected_DefaultsTo5s(t *testing.T) {
	c := New(config.Config{}, logging.NoOp())
	delay := c.nextRetryDelay(fmt.Errorf("%w", errAuthRejected), 0)
	assert.Equal(t, 5*time.Second, delay)
}

func TestNextRetryDelay_AuthRejected_Cap60s(t *testing.T) {
	c := New(config.Config{}, logging.NoOp())
	delay := c.nextRetryDelay(fmt.Errorf("%w", errAuthRejected), 100)
	assert.Equal(t, 60*time.Second, delay)
}

func TestNextRetryDelay_TokenExpired_NoFile_SlowRetry(t *testing.T) {
	c := New(config.Config{}, logging.NoOp())
	delay := c.nextRetryDelay(fmt.Errorf("%w", errTokenExpired), 0)
	assert.Equal(t, 15*time.Second, delay)
}

func TestNextRetryDelay_TokenExpired_WithFile_FastRetry(t *testing.T) {
	c := New(config.Config{BearerTokenFile: "/some/token.jwt"}, logging.NoOp())
	delay := c.nextRetryDelay(fmt.Errorf("%w", errTokenExpired), 0)
	assert.Equal(t, time.Second, delay)
}

func TestNextRetryDelay_TokenRefreshRequired_NoFile_SlowRetry(t *testing.T) {
	c := New(config.Config{}, logging.NoOp())
	delay := c.nextRetryDelay(fmt.Errorf("%w", errTokenRefreshRequired), 0)
	assert.Equal(t, 15*time.Second, delay)
}

func TestNextRetryDelay_TokenRefreshRequired_WithFile_FastRetry(t *testing.T) {
	c := New(config.Config{BearerTokenFile: "/some/token.jwt"}, logging.NoOp())
	delay := c.nextRetryDelay(fmt.Errorf("%w", errTokenRefreshRequired), 0)
	assert.Equal(t, time.Second, delay)
}

func TestNextRetryDelay_TokenNotYetValid(t *testing.T) {
	c := New(config.Config{}, logging.NoOp())
	delay := c.nextRetryDelay(fmt.Errorf("%w", errTokenNotYetValid), 0)
	assert.Equal(t, 2*time.Second, delay)
}

func TestNextRetryDelay_NegativeFailuresClampedToZero(t *testing.T) {
	c := New(config.Config{RetryInterval: time.Second}, logging.NoOp())
	assert.Equal(t, time.Second, c.nextRetryDelay(fmt.Errorf("err"), -1))
}

// ---- handleControl ---------------------------------------------------------

func TestHandleControl_RegisterAck_SetsSessionID(t *testing.T) {
	c := clientWithCh(make(chan []byte, 8))
	c.handleControl(protocol.ControlFrame{Type: protocol.FrameRegisterAck, SessionID: "sess-abc"})
	assert.Equal(t, "sess-abc", c.getLastSessionID())
}

func TestHandleControl_Heartbeat_SendsAck(t *testing.T) {
	ch := make(chan []byte, 8)
	clientWithCh(ch).handleControl(protocol.ControlFrame{Type: protocol.FrameHeartbeat})
	cf := decodeControlMsg(t, ch)
	assert.Equal(t, protocol.FrameHeartbeatAck, cf.Type)
}

func TestHandleControl_Open_RegistersStreamAndSendsOpenAck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			// Keep open until the test closes the client.
			time.Sleep(500 * time.Millisecond)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	ch := make(chan []byte, 8)
	c := clientWithCh(ch)
	c.handleControl(protocol.ControlFrame{
		Type:     protocol.FrameOpen,
		StreamID: 1,
		Target:   ln.Addr().String(),
	})

	// First message must be open_ack.
	cf := decodeControlMsg(t, ch)
	assert.Equal(t, protocol.FrameOpenAck, cf.Type)
	assert.EqualValues(t, 1, cf.StreamID)

	_, ok := c.getLocalConn(1)
	assert.True(t, ok)
	c.closeAllLocalConns()
}

func TestHandleControl_Open_UnreachableTarget_SendsErrorFrame(t *testing.T) {
	addr := unusedAddr(t)
	ch := make(chan []byte, 8)
	clientWithCh(ch).handleControl(protocol.ControlFrame{
		Type:     protocol.FrameOpen,
		StreamID: 2,
		Target:   addr,
	})

	cf := decodeControlMsg(t, ch)
	assert.Equal(t, protocol.FrameError, cf.Type)
}

func TestHandleControl_Open_UsesLocalTargetWhenTargetEmpty(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			time.Sleep(200 * time.Millisecond)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	ch := make(chan []byte, 8)
	c := New(config.Config{LocalTarget: ln.Addr().String()}, logging.NoOp())
	c.setSendChan(ch)
	// Target field is empty — should fall back to cfg.LocalTarget.
	c.handleControl(protocol.ControlFrame{Type: protocol.FrameOpen, StreamID: 10, Target: ""})

	cf := decodeControlMsg(t, ch)
	assert.Equal(t, protocol.FrameOpenAck, cf.Type)
	c.closeAllLocalConns()
}

func TestHandleControl_Close_RemovesLocalConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				time.Sleep(200 * time.Millisecond)
				_ = c.Close()
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	c := New(config.Config{}, logging.NoOp())
	require.NoError(t, c.openLocalConn(99, ln.Addr().String()))
	_, ok := c.getLocalConn(99)
	assert.True(t, ok)

	c.handleControl(protocol.ControlFrame{Type: protocol.FrameClose, StreamID: 99})
	_, ok = c.getLocalConn(99)
	assert.False(t, ok)
}

// ---- handleData ------------------------------------------------------------

func TestHandleData_KnownStream_WritesToLocalConn(t *testing.T) {
	server, client := localConnPair(t)
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})

	c := New(config.Config{}, logging.NoOp())
	c.connMu.Lock()
	c.conns[5] = client
	c.connMu.Unlock()

	payload := []byte("hello from tunnel")
	c.handleData(5, payload)

	buf := make([]byte, len(payload))
	_ = server.SetDeadline(time.Now().Add(time.Second))
	n, err := server.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, payload, buf[:n])

	c.closeAllLocalConns()
}

func TestHandleData_UnknownStream_IsNoOp(t *testing.T) {
	c := New(config.Config{}, logging.NoOp())
	// Must not panic or error.
	c.handleData(999, []byte("data for nobody"))
}

// ---- enqueue ---------------------------------------------------------------

func TestEnqueue_NoSendChan_ReturnsError(t *testing.T) {
	c := New(config.Config{}, logging.NoOp())
	err := c.enqueue([]byte("data"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no active websocket writer")
}

func TestEnqueue_WithSendChan_Succeeds(t *testing.T) {
	ch := make(chan []byte, 8)
	c := clientWithCh(ch)
	require.NoError(t, c.enqueue([]byte("data")))
	assert.Len(t, ch, 1)
}

func TestEnqueue_FullChannel_ReturnsError(t *testing.T) {
	ch := make(chan []byte, 1)
	ch <- []byte("occupying the slot")
	c := clientWithCh(ch)
	err := c.enqueue([]byte("overflow"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "send queue full")
}

func TestEnqueue_AfterClearSendChan_ReturnsError(t *testing.T) {
	ch := make(chan []byte, 8)
	c := clientWithCh(ch)
	c.clearSendChan(ch)
	err := c.enqueue([]byte("data"))
	require.Error(t, err)
}

// ---- Run validation --------------------------------------------------------

func TestRun_MissingServerURL_ReturnsError(t *testing.T) {
	c := New(config.Config{LocalTarget: "127.0.0.1:5432"}, logging.NoOp())
	err := c.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server URL")
}

func TestRun_MissingLocalTarget_ReturnsError(t *testing.T) {
	c := New(config.Config{ServerURL: "ws://127.0.0.1:9999/ws"}, logging.NoOp())
	err := c.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local target")
}

func TestRun_ContextAlreadyCancelled_ReturnsNil(t *testing.T) {
	// A cancelled context should cause Run to return nil (clean shutdown),
	// not an error, even when the server is unreachable.
	c := New(config.Config{
		ServerURL:   "ws://127.0.0.1:19999/ws",
		LocalTarget: "127.0.0.1:5432",
		BearerToken: makeExpiredJWT(t, time.Time{}), // no expiry
	}, logging.NoOp())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Run(ctx)
	assert.NoError(t, err)
}
