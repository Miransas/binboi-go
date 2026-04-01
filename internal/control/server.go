package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sardorazimov/binboi-go/internal/session"
	"github.com/sardorazimov/binboi-go/pkg/api"
)

// ServerConfig contains the daemon's HTTP and stream control-plane settings.
type ServerConfig struct {
	HTTPAddress       string
	ProtocolAddress   string
	HeartbeatInterval time.Duration
	Name              string
	Version           string
}

// Server exposes the daemon's HTTP API and stream control protocol.
type Server struct {
	logger         *slog.Logger
	manager        *session.Manager
	cfg            ServerConfig
	httpServer     *http.Server
	protocolServer *protocolServer
}

// NewServer constructs a new control-plane server.
func NewServer(cfg ServerConfig, logger *slog.Logger, manager *session.Manager) *Server {
	server := &Server{
		logger:  logger,
		manager: manager,
		cfg:     cfg,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealth)
	mux.HandleFunc("/v1/sessions", server.handleSessions)
	mux.HandleFunc("/", server.handleTunnelRequest)

	server.httpServer = &http.Server{
		Addr:              cfg.HTTPAddress,
		Handler:           loggingMiddleware(logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	server.protocolServer = newProtocolServer(cfg.ProtocolAddress, cfg.HeartbeatInterval, logger, manager)

	return server
}

// Handler returns the configured HTTP handler. It is primarily useful for tests.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Run starts both control-plane listeners and shuts them down when the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.runHTTP(ctx); err != nil {
			errCh <- fmt.Errorf("http control server: %w", err)
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.protocolServer.Run(ctx); err != nil {
			errCh <- fmt.Errorf("stream control server: %w", err)
			cancel()
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case err := <-errCh:
		<-done
		return err
	}

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (s *Server) runHTTP(ctx context.Context) error {
	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := s.httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("control server shutdown error", "error", err)
		}
	}()

	err := s.httpServer.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, api.HealthResponse{
		Name:    s.cfg.Name,
		Status:  "ok",
		Version: s.cfg.Version,
		Time:    time.Now().UTC(),
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, api.ListSessionsResponse{
			Sessions: s.manager.List(r.Context()),
		})
	case http.MethodPost:
		var req api.CreateSessionRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid session payload")
			return
		}

		session, err := s.manager.Create(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		writeJSON(w, http.StatusCreated, api.CreateSessionResponse{Session: session})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleTunnelRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		writeError(w, http.StatusNotImplemented, "CONNECT is not supported yet")
		return
	}

	request, err := requestFromHTTP(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	host := normalizeHost(r.Host)
	s.logger.Info("incoming tunneled request",
		"host", host,
		"method", request.Method,
		"path", request.Path,
	)

	response, err := s.protocolServer.ForwardRequest(r.Context(), host, request)
	if err != nil {
		switch {
		case errors.Is(err, errTunnelNotFound):
			writeError(w, http.StatusNotFound, "no active tunnel for host")
		case errors.Is(err, errTunnelUnsupported):
			writeError(w, http.StatusNotImplemented, "tunnel protocol does not support HTTP forwarding yet")
		case errors.Is(err, context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, "tunnel request timed out")
		case errors.Is(err, context.Canceled):
			writeError(w, http.StatusGatewayTimeout, "client closed request")
		default:
			s.logger.Warn("failed to forward request",
				"host", host,
				"method", request.Method,
				"path", request.Path,
				"error", err,
			)
			writeError(w, http.StatusBadGateway, "failed to forward tunneled request")
		}
		return
	}

	s.logger.Info("sending tunneled response",
		"host", host,
		"method", request.Method,
		"path", request.Path,
		"status", response.Status,
	)
	writeForwardedHTTPResponse(w, response)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, api.ErrorResponse{Error: message})
}

func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(rec, r)

		logger.Info("control request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
			"remote_addr", r.RemoteAddr,
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
