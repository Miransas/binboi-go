package control

import (
	"context"
	"encoding/json"
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
	stream := newProtocolServer("127.0.0.1:0", time.Second, logger, manager)

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
