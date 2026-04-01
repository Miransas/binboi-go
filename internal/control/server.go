package control

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sardorazimov/binboi-go/internal/session"
	"github.com/sardorazimov/binboi-go/pkg/api"
)

// Server exposes the control-plane API for the Binboi daemon.
type Server struct {
	logger     *slog.Logger
	manager    *session.Manager
	name       string
	version    string
	httpServer *http.Server
}

// NewServer constructs a new control-plane server.
func NewServer(addr, name, version string, logger *slog.Logger, manager *session.Manager) *Server {
	server := &Server{
		logger:  logger,
		manager: manager,
		name:    name,
		version: version,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealth)
	mux.HandleFunc("/v1/sessions", server.handleSessions)

	server.httpServer = &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	return server
}

// Handler returns the configured HTTP handler. It is primarily useful for tests.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Run starts the server and shuts it down when the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
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
		Name:    s.name,
		Status:  "ok",
		Version: s.version,
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
