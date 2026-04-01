package control

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sardorazimov/binboi-go/internal/auth"
	"github.com/sardorazimov/binboi-go/internal/session"
	"github.com/sardorazimov/binboi-go/pkg/api"
	"github.com/sardorazimov/binboi-go/pkg/client"
)

func TestServerSessionLifecycle(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(ServerConfig{
		HTTPAddress:       ":0",
		ProtocolAddress:   "127.0.0.1:0",
		HeartbeatInterval: time.Second,
		Name:              "binboid",
		Version:           "test",
	}, logger, manager)

	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResp := httptest.NewRecorder()
	server.Handler().ServeHTTP(healthResp, healthReq)

	if healthResp.Code != http.StatusOK {
		t.Fatalf("health status code mismatch: got %d want %d", healthResp.Code, http.StatusOK)
	}

	var health api.HealthResponse
	if err := json.NewDecoder(healthResp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.Status != "ok" {
		t.Fatalf("health status mismatch: got %q want ok", health.Status)
	}

	body := `{"name":"demo","target":"http://127.0.0.1:3000"}`
	sessionReq := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(body))
	sessionReq.Header.Set("Content-Type", "application/json")
	sessionResp := httptest.NewRecorder()
	server.Handler().ServeHTTP(sessionResp, sessionReq)

	if sessionResp.Code != http.StatusCreated {
		t.Fatalf("session status code mismatch: got %d want %d", sessionResp.Code, http.StatusCreated)
	}

	var created api.CreateSessionResponse
	if err := json.NewDecoder(sessionResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create session response: %v", err)
	}
	if created.Session.ID == "" {
		t.Fatal("expected session ID")
	}
	if created.Session.Connection != "idle" {
		t.Fatalf("connection state mismatch: got %q want idle", created.Session.Connection)
	}
}

func TestProtocolRegisterDisconnectAndResume(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	stream := newProtocolServer("127.0.0.1:0", time.Second, api.FlowControl{}.Normalize(), logger, manager, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- stream.Run(ctx)
	}()

	address := waitForAddress(t, stream)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	registeredCh := make(chan api.RegisteredPayload, 4)
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelClient.Run(runCtx, api.RegisterPayload{
			Protocol:  "http",
			LocalPort: 3000,
			Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
		}, func(registered api.RegisteredPayload) {
			registeredCh <- registered
		})
	}()

	initial := waitForRegistered(t, registeredCh)
	if initial.Resumed {
		t.Fatal("expected first registration to be fresh")
	}

	waitForConnectionState(t, manager, initial.TunnelID, "connected")

	if err := stream.closePeer(initial.TunnelID); err != nil {
		t.Fatalf("close peer: %v", err)
	}

	waitForConnectionState(t, manager, initial.TunnelID, "disconnected")

	resumed := waitForRegistered(t, registeredCh)
	if !resumed.Resumed {
		t.Fatal("expected resumed registration")
	}
	if resumed.TunnelID != initial.TunnelID {
		t.Fatalf("tunnel ID mismatch after resume: got %q want %q", resumed.TunnelID, initial.TunnelID)
	}
	if resumed.PublicURL != initial.PublicURL {
		t.Fatalf("public URL mismatch after resume: got %q want %q", resumed.PublicURL, initial.PublicURL)
	}

	waitForConnectionState(t, manager, initial.TunnelID, "connected")

	stop()
	if err := <-runDone; err != nil {
		t.Fatalf("run tunnel client: %v", err)
	}

	waitForConnectionState(t, manager, initial.TunnelID, "disconnected")

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestProtocolRegisterRequiresValidToken(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")

	store := auth.NewStore(t.TempDir() + "/tokens.json")
	if err := store.Ensure(); err != nil {
		t.Fatalf("ensure token store: %v", err)
	}
	created, err := store.CreateToken(context.Background(), "user-123")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	validator := auth.NewValidator(store, auth.ValidatorConfig{
		CacheTTL:               time.Second,
		LastUsedUpdateInterval: time.Millisecond,
	}, logger)

	stream := newProtocolServer("127.0.0.1:0", time.Second, api.FlowControl{}.Normalize(), logger, manager, validator)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- stream.Run(ctx)
	}()

	address := waitForAddress(t, stream)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: 3000,
		AuthToken: created.RawToken,
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err != nil {
		t.Fatalf("connect with valid token: %v", err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	waitForConnectionState(t, manager, tunnelSession.Registered().TunnelID, "connected")

	sessions := manager.List(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("session count mismatch: got %d want 1", len(sessions))
	}
	if sessions[0].UserID != "user-123" {
		t.Fatalf("user ID mismatch: got %q want %q", sessions[0].UserID, "user-123")
	}

	stop()
	if err := <-runDone; err != nil {
		t.Fatalf("run tunnel session: %v", err)
	}

	_, err = tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: 3000,
		AuthToken: "bin_live_dead_invalid",
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err == nil {
		t.Fatal("expected invalid token error")
	}
	var protocolErr *client.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("expected protocol error, got %T", err)
	}
	if protocolErr.Code != "invalid_token" {
		t.Fatalf("protocol error code mismatch: got %q want %q", protocolErr.Code, "invalid_token")
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestHTTPForwardingConcurrentRequests(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(ServerConfig{
		HTTPAddress:       ":0",
		ProtocolAddress:   "127.0.0.1:0",
		HeartbeatInterval: time.Second,
		Name:              "binboid",
		Version:           "test",
	}, logger, manager)

	upstreamURL, shutdownUpstream := startUpstreamHTTPServer(t, 75*time.Millisecond)
	defer shutdownUpstream()

	localURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	localPort, err := portFromHost(localURL.Host)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.protocolServer.Run(ctx)
	}()

	address := waitForAddress(t, server.protocolServer)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: localPort,
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err != nil {
		t.Fatalf("connect tunnel client: %v", err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	publicHost := hostNameFromPublicURL(tunnelSession.Registered().PublicURL)
	if publicHost == "" {
		t.Fatal("expected public host")
	}

	paths := []string{"/alpha?x=1", "/beta", "/gamma", "/delta"}
	recorders := make([]*httptest.ResponseRecorder, len(paths))
	var wg sync.WaitGroup

	for i, path := range paths {
		i, path := i, path
		wg.Add(1)
		go func() {
			defer wg.Done()

			requestBody := strings.Repeat("hello-from-request-", 4096)
			req := httptest.NewRequest(http.MethodPost, "http://"+publicHost+path, strings.NewReader(requestBody))
			req.Host = publicHost
			req.Header.Set("X-Test-Header", "binboi")
			req.Header.Set("X-Response-Bytes", "131072")

			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, req)
			recorders[i] = recorder
		}()
	}

	wg.Wait()

	for i, recorder := range recorders {
		if recorder.Code != http.StatusCreated {
			t.Fatalf("response status mismatch for request %d: got %d want %d", i, recorder.Code, http.StatusCreated)
		}
		if got := recorder.Header().Get("X-Upstream-Service"); got != "http-basic-test" {
			t.Fatalf("missing upstream header for request %d: got %q", i, got)
		}
		if !strings.Contains(recorder.Body.String(), paths[i]) {
			t.Fatalf("response body missing path for request %d: %q", i, recorder.Body.String())
		}
		if len(recorder.Body.String()) < 131072 {
			t.Fatalf("expected large streamed response body for request %d, got %d bytes", i, len(recorder.Body.String()))
		}
	}

	waitForInFlight(t, manager, tunnelSession.Registered().TunnelID, 0)

	stop()
	if err := <-runDone; err != nil {
		t.Fatalf("run tunnel session: %v", err)
	}

	waitForConnectionState(t, manager, tunnelSession.Registered().TunnelID, "disconnected")

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestPendingRequestsFailOnDisconnect(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(ServerConfig{
		HTTPAddress:       ":0",
		ProtocolAddress:   "127.0.0.1:0",
		HeartbeatInterval: time.Second,
		Name:              "binboid",
		Version:           "test",
	}, logger, manager)

	upstreamURL, shutdownUpstream := startUpstreamHTTPServer(t, 2*time.Second)
	defer shutdownUpstream()

	localURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	localPort, err := portFromHost(localURL.Host)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.protocolServer.Run(ctx)
	}()

	address := waitForAddress(t, server.protocolServer)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: localPort,
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err != nil {
		t.Fatalf("connect tunnel client: %v", err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	publicHost := hostNameFromPublicURL(tunnelSession.Registered().PublicURL)
	req := httptest.NewRequest(http.MethodGet, "http://"+publicHost+"/slow", nil)
	req.Host = publicHost

	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, req)
		respCh <- recorder
	}()

	waitForInFlightAtLeast(t, manager, tunnelSession.Registered().TunnelID, 1)

	if err := server.protocolServer.closePeer(tunnelSession.Registered().TunnelID); err != nil {
		t.Fatalf("close peer: %v", err)
	}

	recorder := <-respCh
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected pending request failure to return 503, got %d", recorder.Code)
	}

	waitForInFlight(t, manager, tunnelSession.Registered().TunnelID, 0)

	stop()
	_ = <-runDone

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestRequestCancellationPropagatesToUpstream(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(ServerConfig{
		HTTPAddress:       ":0",
		ProtocolAddress:   "127.0.0.1:0",
		HeartbeatInterval: time.Second,
		Name:              "binboid",
		Version:           "test",
	}, logger, manager)

	upstreamURL, upstreamCanceled, shutdownUpstream := startCancelableUpstreamServer(t)
	defer shutdownUpstream()

	localURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	localPort, err := portFromHost(localURL.Host)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.protocolServer.Run(ctx)
	}()

	address := waitForAddress(t, server.protocolServer)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: localPort,
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err != nil {
		t.Fatalf("connect tunnel client: %v", err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	publicHost := hostNameFromPublicURL(tunnelSession.Registered().PublicURL)
	reqCtx, reqCancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://"+publicHost+"/cancel", strings.NewReader(strings.Repeat("cancel-me-", 2048))).WithContext(reqCtx)
	req.Host = publicHost

	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, req)
		respCh <- recorder
	}()

	waitForInFlightAtLeast(t, manager, tunnelSession.Registered().TunnelID, 1)
	reqCancel()

	select {
	case <-upstreamCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream cancellation")
	}

	recorder := <-respCh
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected canceled request to return 504, got %d", recorder.Code)
	}

	waitForInFlight(t, manager, tunnelSession.Registered().TunnelID, 0)

	stop()
	_ = <-runDone

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestWebSocketUpgradeEchoAndMixedTraffic(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(ServerConfig{
		HTTPAddress:       ":0",
		ProtocolAddress:   "127.0.0.1:0",
		HeartbeatInterval: time.Second,
		Name:              "binboid",
		Version:           "test",
	}, logger, manager)

	upstreamURL, shutdownUpstream := startHTTPAndWebSocketEchoServer(t)
	defer shutdownUpstream()

	localURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	localPort, err := portFromHost(localURL.Host)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.protocolServer.Run(ctx)
	}()

	address := waitForAddress(t, server.protocolServer)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: localPort,
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err != nil {
		t.Fatalf("connect tunnel client: %v", err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	publicHost := hostNameFromPublicURL(tunnelSession.Registered().PublicURL)
	externalHTTP := httptest.NewServer(server.Handler())
	defer externalHTTP.Close()

	type websocketResult struct {
		payload string
		err     error
	}

	results := make(chan websocketResult, 2)
	for _, payload := range []string{"hello-over-ws", "second-websocket"} {
		payload := payload
		go func() {
			conn, reader, err := dialWebSocket(t, externalHTTP.URL, publicHost, "/ws")
			if err != nil {
				results <- websocketResult{err: err}
				return
			}
			defer conn.Close()

			if err := writeWebSocketFrame(conn, 0x1, []byte(payload), true); err != nil {
				results <- websocketResult{err: err}
				return
			}

			opcode, echoed, err := readWebSocketFrame(reader)
			if err != nil {
				results <- websocketResult{err: err}
				return
			}
			if opcode != 0x1 {
				results <- websocketResult{err: fmt.Errorf("unexpected opcode %d", opcode)}
				return
			}

			_ = writeWebSocketFrame(conn, 0x8, []byte{}, true)
			results <- websocketResult{payload: string(echoed)}
		}()
	}

	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("websocket echo failed: %v", result.err)
		}
		if result.payload != "hello-over-ws" && result.payload != "second-websocket" {
			t.Fatalf("unexpected websocket echo payload: %q", result.payload)
		}
	}

	httpReq, err := http.NewRequest(http.MethodPost, externalHTTP.URL+"/http-check", strings.NewReader("plain-http-body"))
	if err != nil {
		t.Fatalf("create tunneled HTTP request: %v", err)
	}
	httpReq.Host = publicHost

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("execute tunneled HTTP request: %v", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatalf("read tunneled HTTP response: %v", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected tunneled HTTP status: got %d want %d", httpResp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(string(body), "/http-check") {
		t.Fatalf("expected tunneled HTTP response to contain path, got %q", string(body))
	}

	stop()
	if err := <-runDone; err != nil {
		t.Fatalf("run tunnel session: %v", err)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestWebSocketUpgradeDisconnectClosesConnection(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(ServerConfig{
		HTTPAddress:       ":0",
		ProtocolAddress:   "127.0.0.1:0",
		HeartbeatInterval: time.Second,
		Name:              "binboid",
		Version:           "test",
	}, logger, manager)

	upstreamURL, shutdownUpstream := startHTTPAndWebSocketEchoServer(t)
	defer shutdownUpstream()

	localURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	localPort, err := portFromHost(localURL.Host)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.protocolServer.Run(ctx)
	}()

	address := waitForAddress(t, server.protocolServer)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: localPort,
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err != nil {
		t.Fatalf("connect tunnel client: %v", err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	publicHost := hostNameFromPublicURL(tunnelSession.Registered().PublicURL)
	externalHTTP := httptest.NewServer(server.Handler())
	defer externalHTTP.Close()

	conn, reader, err := dialWebSocket(t, externalHTTP.URL, publicHost, "/disconnect")
	if err != nil {
		t.Fatalf("dial tunneled websocket: %v", err)
	}
	defer conn.Close()

	if err := writeWebSocketFrame(conn, 0x1, []byte("before-disconnect"), true); err != nil {
		t.Fatalf("write tunneled websocket frame: %v", err)
	}
	if _, echoed, err := readWebSocketFrame(reader); err != nil {
		t.Fatalf("read tunneled websocket echo: %v", err)
	} else if string(echoed) != "before-disconnect" {
		t.Fatalf("unexpected websocket echo before disconnect: %q", string(echoed))
	}

	if err := server.protocolServer.closePeer(tunnelSession.Registered().TunnelID); err != nil {
		t.Fatalf("close peer: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := readWebSocketFrame(reader); err == nil {
		t.Fatal("expected tunneled websocket to close after peer disconnect")
	}

	stop()
	_ = <-runDone

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestFlowControlQueuesWhenStreamLimitIsReached(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(ServerConfig{
		HTTPAddress:       ":0",
		ProtocolAddress:   "127.0.0.1:0",
		HeartbeatInterval: time.Second,
		FlowControl: api.FlowControl{
			MaxConcurrentStreams:     1,
			BufferedBytesPerStream:   api.DefaultBodyChunkSize,
			StreamTimeoutSeconds:     10,
			StreamIdleTimeoutSeconds: 2,
		},
		Name:    "binboid",
		Version: "test",
	}, logger, manager)

	upstreamURL, shutdownUpstream := startUpstreamHTTPServer(t, 175*time.Millisecond)
	defer shutdownUpstream()

	localURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	localPort, err := portFromHost(localURL.Host)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.protocolServer.Run(ctx)
	}()

	address := waitForAddress(t, server.protocolServer)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: localPort,
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err != nil {
		t.Fatalf("connect tunnel client: %v", err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	publicHost := hostNameFromPublicURL(tunnelSession.Registered().PublicURL)
	startedAt := time.Now()

	var wg sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, 2)
	for i := range recorders {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()

			req := httptest.NewRequest(http.MethodGet, "http://"+publicHost+"/queued-"+strconv.Itoa(i), nil)
			req.Host = publicHost

			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, req)
			recorders[i] = recorder
		}()
	}

	wg.Wait()

	if elapsed := time.Since(startedAt); elapsed < 300*time.Millisecond {
		t.Fatalf("expected queued requests to take at least 300ms with one active stream, got %v", elapsed)
	}

	for i, recorder := range recorders {
		if recorder.Code != http.StatusCreated {
			t.Fatalf("response status mismatch for request %d: got %d want %d", i, recorder.Code, http.StatusCreated)
		}
	}

	waitForInFlight(t, manager, tunnelSession.Registered().TunnelID, 0)

	stop()
	_ = <-runDone

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestIdleTimeoutReturnsGatewayTimeout(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(ServerConfig{
		HTTPAddress:       ":0",
		ProtocolAddress:   "127.0.0.1:0",
		HeartbeatInterval: time.Second,
		FlowControl: api.FlowControl{
			MaxConcurrentStreams:     4,
			BufferedBytesPerStream:   api.DefaultBodyChunkSize,
			StreamTimeoutSeconds:     5,
			StreamIdleTimeoutSeconds: 1,
		},
		Name:    "binboid",
		Version: "test",
	}, logger, manager)

	upstreamURL, shutdownUpstream := startUpstreamHTTPServer(t, 2*time.Second)
	defer shutdownUpstream()

	localURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	localPort, err := portFromHost(localURL.Host)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.protocolServer.Run(ctx)
	}()

	address := waitForAddress(t, server.protocolServer)
	tunnelClient, err := client.NewTunnel(address, logger)
	if err != nil {
		t.Fatalf("create tunnel client: %v", err)
	}

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: localPort,
		Metadata:  api.ClientMetadata{ClientVersion: "test", Hostname: "unit-test"},
	})
	if err != nil {
		t.Fatalf("connect tunnel client: %v", err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	publicHost := hostNameFromPublicURL(tunnelSession.Registered().PublicURL)
	req := httptest.NewRequest(http.MethodGet, "http://"+publicHost+"/idle", nil)
	req.Host = publicHost

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected idle timeout to return 504, got %d", recorder.Code)
	}

	waitForInFlight(t, manager, tunnelSession.Registered().TunnelID, 0)

	stop()
	_ = <-runDone

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func waitForAddress(t *testing.T, stream *protocolServer) string {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if address := stream.Address(); address != "" {
			return address
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("protocol server did not publish an address")
	return ""
}

func waitForRegistered(t *testing.T, registeredCh <-chan api.RegisteredPayload) api.RegisteredPayload {
	t.Helper()

	select {
	case registered := <-registeredCh:
		return registered
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for registered payload")
		return api.RegisteredPayload{}
	}
}

func waitForConnectionState(t *testing.T, manager *session.Manager, sessionID, state string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, current := range manager.List(context.Background()) {
			if current.ID == sessionID && current.Connection == state {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for session %s to reach connection state %q", sessionID, state)
}

func waitForInFlight(t *testing.T, manager *session.Manager, sessionID string, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, current := range manager.List(context.Background()) {
			if current.ID == sessionID && current.InFlight == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for session %s to reach in-flight %d", sessionID, want)
}

func waitForInFlightAtLeast(t *testing.T, manager *session.Manager, sessionID string, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, current := range manager.List(context.Background()) {
			if current.ID == sessionID && current.InFlight >= want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for session %s to reach in-flight >= %d", sessionID, want)
}

func startUpstreamHTTPServer(t *testing.T, delay time.Duration) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream server: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		time.Sleep(delay)
		w.Header().Set("X-Upstream-Service", "http-basic-test")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, r.URL.RequestURI()+"|"+string(body))
		if size := r.Header.Get("X-Response-Bytes"); size != "" {
			if n, err := strconv.Atoi(size); err == nil && n > 0 {
				_, _ = io.WriteString(w, strings.Repeat("r", n))
			}
		}
	})

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener)
	}()

	return "http://" + listener.Addr().String(), func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
}

func startCancelableUpstreamServer(t *testing.T) (string, <-chan struct{}, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen cancelable upstream server: %v", err)
	}

	canceled := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
		canceled <- struct{}{}
	})

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener)
	}()

	return "http://" + listener.Addr().String(), canceled, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
}

func startHTTPAndWebSocketEchoServer(t *testing.T) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen websocket echo server: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if api.UpgradeTypeFromHeaders(r.Header) == "websocket" {
			handleWebSocketEchoUpgrade(t, w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream-Service", "http-ws-test")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.URL.RequestURI()+"|"+string(body))
	})

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener)
	}()

	return "http://" + listener.Addr().String(), func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
}

func handleWebSocketEchoUpgrade(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("response writer does not support hijacking")
	}

	conn, rw, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("hijack websocket echo connection: %v", err)
	}
	defer conn.Close()

	key := r.Header.Get("Sec-WebSocket-Key")
	accept := websocketAccept(key)
	if _, err := fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		t.Fatalf("write websocket status line: %v", err)
	}
	if _, err := fmt.Fprintf(rw, "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
		t.Fatalf("write websocket headers: %v", err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatalf("flush websocket headers: %v", err)
	}

	reader := rw.Reader
	for {
		opcode, payload, err := readWebSocketFrame(reader)
		if err != nil {
			return
		}
		switch opcode {
		case 0x8:
			_ = writeWebSocketFrame(conn, 0x8, payload, false)
			return
		case 0x9:
			_ = writeWebSocketFrame(conn, 0xA, payload, false)
		default:
			_ = writeWebSocketFrame(conn, opcode, payload, false)
		}
	}
}

func dialWebSocket(t *testing.T, serverURL, host, path string) (net.Conn, *bufio.Reader, error) {
	t.Helper()

	parsedURL, err := url.Parse(serverURL)
	if err != nil {
		return nil, nil, err
	}

	conn, err := net.Dial("tcp", parsedURL.Host)
	if err != nil {
		return nil, nil, err
	}

	key := base64.StdEncoding.EncodeToString([]byte("binboi-websocket"))
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", path, host, key)
	if _, err := io.WriteString(conn, request); err != nil {
		conn.Close()
		return nil, nil, err
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, nil, fmt.Errorf("unexpected websocket handshake status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != websocketAccept(key) {
		conn.Close()
		return nil, nil, fmt.Errorf("unexpected Sec-WebSocket-Accept header %q", got)
	}

	return conn, reader, nil
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func writeWebSocketFrame(w io.Writer, opcode byte, payload []byte, masked bool) error {
	firstByte := byte(0x80 | (opcode & 0x0F))
	if _, err := w.Write([]byte{firstByte}); err != nil {
		return err
	}

	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}

	payloadLen := len(payload)
	switch {
	case payloadLen < 126:
		if _, err := w.Write([]byte{maskBit | byte(payloadLen)}); err != nil {
			return err
		}
	case payloadLen <= 65535:
		if _, err := w.Write([]byte{maskBit | 126, byte(payloadLen >> 8), byte(payloadLen)}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("payload too large for test frame: %d", payloadLen)
	}

	if !masked {
		_, err := w.Write(payload)
		return err
	}

	maskKey := []byte{0x11, 0x22, 0x33, 0x44}
	if _, err := w.Write(maskKey); err != nil {
		return err
	}

	maskedPayload := make([]byte, len(payload))
	for i, b := range payload {
		maskedPayload[i] = b ^ maskKey[i%len(maskKey)]
	}
	_, err := w.Write(maskedPayload)
	return err
}

func readWebSocketFrame(r io.Reader) (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}

	opcode := header[0] & 0x0F
	masked := header[1]&0x80 != 0
	payloadLen := int(header[1] & 0x7F)

	switch payloadLen {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(r, extended); err != nil {
			return 0, nil, err
		}
		payloadLen = int(extended[0])<<8 | int(extended[1])
	case 127:
		return 0, nil, fmt.Errorf("unsupported 64-bit websocket payload length in test helper")
	}

	var maskKey []byte
	if masked {
		maskKey = make([]byte, 4)
		if _, err := io.ReadFull(r, maskKey); err != nil {
			return 0, nil, err
		}
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%len(maskKey)]
		}
	}
	return opcode, payload, nil
}

func portFromHost(host string) (int, error) {
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(port)
}

func hostNameFromPublicURL(publicURL string) string {
	parsed, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
