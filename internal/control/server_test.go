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
}

func TestProtocolRegisterAndHeartbeat(t *testing.T) {
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

	tunnelSession, err := tunnelClient.Connect(context.Background(), api.RegisterPayload{
		Protocol:  "http",
		LocalPort: 3000,
		Metadata: api.ClientMetadata{
			ClientVersion: "test",
			Hostname:      "unit-test",
		},
	})
	if err != nil {
		t.Fatalf("connect tunnel client: %v", err)
	}

	runCtx, stop := context.WithTimeout(context.Background(), 2200*time.Millisecond)
	defer stop()

	runDone := make(chan error, 1)
	go func() {
		runDone <- tunnelSession.Run(runCtx)
	}()

	<-runCtx.Done()

	if err := <-runDone; err != nil {
		t.Fatalf("run tunnel session: %v", err)
	}

	waitForNoSessions(t, manager)

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("protocol server run: %v", err)
	}
}

func TestHTTPForwarding(t *testing.T) {
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

	upstreamURL, shutdownUpstream := startUpstreamHTTPServer(t)
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
		Metadata: api.ClientMetadata{
			ClientVersion: "test",
			Hostname:      "unit-test",
		},
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

	publicHost := hostFromPublicURL(tunnelSession.Registered().PublicURL)
	if publicHost == "" {
		t.Fatal("expected public host")
	}

	paths := []string{"/alpha?x=1", "/beta"}
	recorders := make([]*httptest.ResponseRecorder, len(paths))
	var wg sync.WaitGroup

	for i, path := range paths {
		i, path := i, path
		wg.Add(1)
		go func() {
			defer wg.Done()

			req := httptest.NewRequest(http.MethodPost, "http://"+publicHost+path, strings.NewReader("hello from request"))
			req.Host = publicHost
			req.Header.Set("X-Test-Header", "binboi")

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
		if !strings.Contains(recorder.Body.String(), "hello from request") {
			t.Fatalf("response body missing request payload for request %d: %q", i, recorder.Body.String())
		}
	}

	stop()
	if err := <-runDone; err != nil {
		t.Fatalf("run tunnel session: %v", err)
	}

	waitForNoSessions(t, manager)

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

func waitForNoSessions(t *testing.T, manager *session.Manager) {
	t.Helper()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(manager.List(context.Background())) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("expected active sessions to be removed after disconnect, got %d", len(manager.List(context.Background())))
}

func startUpstreamHTTPServer(t *testing.T) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream server: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream-Service", "http-basic-test")
		w.WriteHeader(http.StatusCreated)
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

func portFromHost(host string) (int, error) {
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(port)
}
