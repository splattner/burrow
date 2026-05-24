package server_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	clientpkg "github.com/splattner/burrow/internal/client"
	"github.com/splattner/burrow/internal/config"
	"github.com/splattner/burrow/internal/logging"
	serverpkg "github.com/splattner/burrow/internal/server"
	"github.com/splattner/burrow/internal/tunnel"
)

func TestServerClientRelayWithLocalEcho(t *testing.T) {
	echoAddr, stopEcho := startTCPEcho(t)
	defer stopEcho()
	token, err := signedHMACTestToken("jwt-secret", "client-a")
	if err != nil {
		t.Fatalf("sign jwt token: %v", err)
	}

	srv := serverpkg.New(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "jwt-secret",
		JWTAudience:   "burrow-server",
		ServerAddr:    "127.0.0.1:0",
		Namespace:     "default",
	}, logging.NoOp())

	srvCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Run(srvCtx)
	}()

	if !srv.WaitUntilStarted(3 * time.Second) {
		t.Fatal("server did not start listening in time")
	}

	metricsURL := strings.Replace(srv.WSURL(), "ws://", "http://", 1)
	metricsURL = strings.TrimSuffix(metricsURL, "/ws") + "/metrics"
	resp, err := http.Get(metricsURL)
	if err != nil {
		t.Fatalf("request metrics endpoint: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected metrics status 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "burrow_sessions_active") {
		t.Fatalf("metrics output missing expected family: %s", string(body))
	}

	cli := clientpkg.New(config.Config{
		BearerToken: token,
		ServerURL:   srv.WSURL(),
		ClientID:    "client-a",
		LocalTarget: echoAddr,
	}, logging.NoOp())

	cliCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()

	clientErr := make(chan error, 1)
	go func() {
		clientErr <- cli.Run(cliCtx)
	}()

	if !srv.WaitForClient("client-a", 5*time.Second) {
		t.Fatal("server did not observe a registered client")
	}

	stream, err := srv.OpenStream("client-a", 100, "")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	want := []byte("ping-through-tunnel")
	if err := srv.SendData("client-a", 100, want); err != nil {
		t.Fatalf("send data over tunnel: %v", err)
	}

	select {
	case got := <-stream.In:
		if string(got) != string(want) {
			t.Fatalf("unexpected echo payload: got=%q want=%q", string(got), string(want))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for echoed payload")
	}

	_ = srv.CloseStream("client-a", 100)

	cancelClient()
	select {
	case err := <-clientErr:
		if err != nil {
			t.Fatalf("client exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not stop after cancellation")
	}

	cancelServer()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}

func TestServerBridgeListenerRelaysToClient(t *testing.T) {
	echoAddr, stopEcho := startTCPEcho(t)
	defer stopEcho()
	token, err := signedHMACTestToken("jwt-secret", "client-a")
	if err != nil {
		t.Fatalf("sign jwt token: %v", err)
	}

	srv := serverpkg.New(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "jwt-secret",
		JWTAudience:   "burrow-server",
		ServerAddr:    "127.0.0.1:0",
		BridgeHost:    "127.0.0.1:0",
		Namespace:     "default",
	}, logging.NoOp())

	srvCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Run(srvCtx)
	}()

	if !srv.WaitUntilStarted(3 * time.Second) {
		t.Fatal("server did not start listening in time")
	}

	cli := clientpkg.New(config.Config{
		BearerToken: token,
		ServerURL:   srv.WSURL(),
		ClientID:    "client-a",
		LocalTarget: echoAddr,
	}, logging.NoOp())

	cliCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()

	clientErr := make(chan error, 1)
	go func() {
		clientErr <- cli.Run(cliCtx)
	}()

	if !srv.WaitForClient("client-a", 5*time.Second) {
		t.Fatal("server did not observe a registered client")
	}

	if srv.BridgeAddr("client-a") == "" {
		t.Fatal("bridge listener did not expose an address")
	}

	podConn, err := net.Dial("tcp", srv.BridgeAddr("client-a"))
	if err != nil {
		t.Fatalf("dial bridge listener: %v", err)
	}
	defer func() {
		_ = podConn.Close()
	}()

	want := []byte("pod-to-client-echo")
	if _, err := podConn.Write(want); err != nil {
		t.Fatalf("write to bridge conn: %v", err)
	}

	got := make([]byte, len(want))
	if _, err := io.ReadFull(podConn, got); err != nil {
		t.Fatalf("read echoed payload from bridge conn: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("unexpected echoed payload: got=%q want=%q", string(got), string(want))
	}

	cancelClient()
	select {
	case err := <-clientErr:
		if err != nil {
			t.Fatalf("client exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not stop after cancellation")
	}

	cancelServer()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}

func TestServerClientReconnectReopensFreshStreams(t *testing.T) {
	echoAddr, stopEcho := startTCPEcho(t)
	defer stopEcho()
	token, err := signedHMACTestToken("jwt-secret", "client-reconnect")
	if err != nil {
		t.Fatalf("sign jwt token: %v", err)
	}

	srv := serverpkg.New(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "jwt-secret",
		JWTAudience:   "burrow-server",
		ServerAddr:    "127.0.0.1:0",
		Namespace:     "default",
	}, logging.NoOp())

	srvCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.Run(srvCtx) }()

	if !srv.WaitUntilStarted(3 * time.Second) {
		t.Fatal("server did not start listening in time")
	}

	clientCfg := config.Config{BearerToken: token, ServerURL: srv.WSURL(), ClientID: "client-reconnect", LocalTarget: echoAddr}

	clientCtx1, cancelClient1 := context.WithCancel(context.Background())
	clientErr1 := make(chan error, 1)
	cli1 := clientpkg.New(clientCfg, logging.NoOp())
	go func() { clientErr1 <- cli1.Run(clientCtx1) }()

	if !srv.WaitForClient("client-reconnect", 5*time.Second) {
		t.Fatal("server did not observe first client registration")
	}

	stream1, err := srv.OpenStream("client-reconnect", 401, "")
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	if err := srv.SendData("client-reconnect", 401, []byte("before-reconnect")); err != nil {
		t.Fatalf("send first stream payload: %v", err)
	}
	select {
	case <-stream1.In:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first stream response")
	}

	cancelClient1()
	select {
	case err := <-clientErr1:
		if err != nil {
			t.Fatalf("first client exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first client did not stop after cancellation")
	}

	select {
	case _, ok := <-stream1.In:
		if ok {
			t.Fatal("expected first stream channel to close after disconnect")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first stream closure after disconnect")
	}

	clientCtx2, cancelClient2 := context.WithCancel(context.Background())
	defer cancelClient2()
	clientErr2 := make(chan error, 1)
	cli2 := clientpkg.New(clientCfg, logging.NoOp())
	go func() { clientErr2 <- cli2.Run(clientCtx2) }()

	var stream2 *tunnel.Stream
	var openErr error
	deadline := time.Now().Add(4 * time.Second)
	for {
		stream, err := srv.OpenStream("client-reconnect", 402, "")
		if err == nil {
			stream2 = stream
			break
		}
		openErr = err
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for reconnect stream open, last err=%v", openErr)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := srv.SendData("client-reconnect", 402, []byte("after-reconnect")); err != nil {
		t.Fatalf("send second stream payload: %v", err)
	}
	select {
	case got := <-stream2.In:
		if string(got) != "after-reconnect" {
			t.Fatalf("unexpected second stream payload: %q", string(got))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for second stream response")
	}

	_ = srv.CloseStream("client-reconnect", 402)

	cancelClient2()
	select {
	case err := <-clientErr2:
		if err != nil {
			t.Fatalf("second client exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second client did not stop after cancellation")
	}

	cancelServer()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}

func TestServerRejectsRegisterWhenJWTSubjectMismatch(t *testing.T) {
	echoAddr, stopEcho := startTCPEcho(t)
	defer stopEcho()

	srv := serverpkg.New(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "jwt-secret",
		ServerAddr:    "127.0.0.1:0",
		Namespace:     "default",
	}, logging.NoOp())

	srvCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Run(srvCtx)
	}()

	if !srv.WaitUntilStarted(3 * time.Second) {
		t.Fatal("server did not start listening in time")
	}

	token, err := signedHMACTestToken("jwt-secret", "different-client")
	if err != nil {
		t.Fatalf("sign jwt token: %v", err)
	}

	cli := clientpkg.New(config.Config{
		BearerToken: token,
		ServerURL:   srv.WSURL(),
		ClientID:    "client-a",
		LocalTarget: echoAddr,
	}, logging.NoOp())

	cliCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()

	clientErr := make(chan error, 1)
	go func() {
		clientErr <- cli.Run(cliCtx)
	}()

	if srv.WaitForClient("client-a", 2*time.Second) {
		t.Fatal("expected server to reject client registration for mismatched jwt subject")
	}

	cancelClient()
	select {
	case err := <-clientErr:
		if err != nil {
			t.Fatalf("client exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not stop after cancellation")
	}

	cancelServer()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}

func startTCPEcho(t *testing.T) (string, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo server: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				if errors.Is(acceptErr, net.ErrClosed) {
					return
				}
				continue
			}

			go func(c net.Conn) {
				defer func() {
					_ = c.Close()
				}()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	stop := func() {
		_ = ln.Close()
		select {
		case <-stopped:
		case <-time.After(1 * time.Second):
		}
	}

	return ln.Addr().String(), stop
}

func signedHMACTestToken(secret, subject string) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    "test-issuer",
		Audience:  jwt.ClaimStrings{"burrow-server"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(2 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		NotBefore: jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		Subject:   subject,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(secret))
}

// TestTwoClientsSimultaneous verifies that the server supports multiple
// concurrent client sessions with fully isolated streams.
func TestTwoClientsSimultaneous(t *testing.T) {
	echoA, stopA := startTCPEcho(t)
	defer stopA()
	echoB, stopB := startTCPEcho(t)
	defer stopB()

	tokenA, err := signedHMACTestToken("jwt-secret", "client-alpha")
	if err != nil {
		t.Fatalf("sign token A: %v", err)
	}
	tokenB, err := signedHMACTestToken("jwt-secret", "client-beta")
	if err != nil {
		t.Fatalf("sign token B: %v", err)
	}

	srv := serverpkg.New(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "jwt-secret",
		JWTAudience:   "burrow-server",
		ServerAddr:    "127.0.0.1:0",
		Namespace:     "default",
	}, logging.NoOp())

	srvCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()

	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.Run(srvCtx) }()

	if !srv.WaitUntilStarted(3 * time.Second) {
		t.Fatal("server did not start listening in time")
	}

	cliA := clientpkg.New(config.Config{BearerToken: tokenA, ServerURL: srv.WSURL(), ClientID: "client-alpha", LocalTarget: echoA}, logging.NoOp())
	cliB := clientpkg.New(config.Config{BearerToken: tokenB, ServerURL: srv.WSURL(), ClientID: "client-beta", LocalTarget: echoB}, logging.NoOp())

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() { errA <- cliA.Run(ctxA) }()
	go func() { errB <- cliB.Run(ctxB) }()

	if !srv.WaitForClient("client-alpha", 5*time.Second) {
		t.Fatal("server did not observe client-alpha registration")
	}
	if !srv.WaitForClient("client-beta", 5*time.Second) {
		t.Fatal("server did not observe client-beta registration")
	}

	// Open isolated streams on each client.
	streamA, err := srv.OpenStream("client-alpha", 201, "")
	if err != nil {
		t.Fatalf("open stream to client-alpha: %v", err)
	}
	streamB, err := srv.OpenStream("client-beta", 202, "")
	if err != nil {
		t.Fatalf("open stream to client-beta: %v", err)
	}

	payloadA := []byte("hello-alpha")
	payloadB := []byte("hello-beta")

	if err := srv.SendData("client-alpha", 201, payloadA); err != nil {
		t.Fatalf("send to client-alpha: %v", err)
	}
	if err := srv.SendData("client-beta", 202, payloadB); err != nil {
		t.Fatalf("send to client-beta: %v", err)
	}

	// Verify each stream gets its own echo back (not the other's payload).
	for _, tc := range []struct {
		name    string
		stream  *tunnel.Stream
		payload []byte
	}{
		{"alpha", streamA, payloadA},
		{"beta", streamB, payloadB},
	} {
		select {
		case got := <-tc.stream.In:
			if string(got) != string(tc.payload) {
				t.Fatalf("client-%s: unexpected echo: got=%q want=%q", tc.name, string(got), string(tc.payload))
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for echo from client-%s", tc.name)
		}
	}

	_ = srv.CloseStream("client-alpha", 201)
	_ = srv.CloseStream("client-beta", 202)

	cancelA()
	cancelB()
	for _, ch := range []chan error{errA, errB} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("client error on stop: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("client did not stop after cancellation")
		}
	}

	cancelServer()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}
