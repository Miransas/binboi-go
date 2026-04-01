package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sardorazimov/binboi-go/internal/proxy"
	"github.com/sardorazimov/binboi-go/internal/transport"
	"github.com/sardorazimov/binboi-go/internal/tunnel"
	"github.com/sardorazimov/binboi-go/pkg/api"
)

// RegisterRequest captures the information needed to create a live control session.
type RegisterRequest struct {
	Protocol  string
	LocalPort int
	AuthToken string
	Metadata  api.ClientMetadata
	Conn      net.Conn
}

type record struct {
	session    api.Session
	conn       net.Conn
	remoteAddr string
	authToken  string
	metadata   api.ClientMetadata
}

// Manager stores session state and prepares public session metadata.
type Manager struct {
	mu       sync.RWMutex
	engine   *tunnel.Engine
	planner  *proxy.Planner
	sessions map[string]*record
}

// NewManager creates an in-memory session manager.
func NewManager(publicHost, forwardedHeader string) *Manager {
	return &Manager{
		engine:   tunnel.NewEngine(publicHost),
		planner:  proxy.NewPlanner(forwardedHeader),
		sessions: make(map[string]*record),
	}
}

// Create validates and stores a session request.
func (m *Manager) Create(_ context.Context, req api.CreateSessionRequest) (api.Session, error) {
	if strings.TrimSpace(req.Name) == "" {
		return api.Session{}, errors.New("session name is required")
	}
	if strings.TrimSpace(req.Target) == "" {
		return api.Session{}, errors.New("session target is required")
	}

	target, err := transport.ParseTarget(req.Target, req.Protocol)
	if err != nil {
		return api.Session{}, err
	}

	return m.storeSession(target, sessionParams{
		name:      req.Name,
		localPort: inferLocalPort(target.URL),
		status:    "ready",
	})
}

// RegisterTunnel validates a stream registration request and stores an active session.
func (m *Manager) RegisterTunnel(_ context.Context, req RegisterRequest) (api.Session, error) {
	if req.Conn == nil {
		return api.Session{}, errors.New("register request requires a live control connection")
	}
	if _, err := api.NormalizeTunnelProtocol(req.Protocol); err != nil {
		return api.Session{}, err
	}

	target, err := transport.LocalTarget(req.Protocol, req.LocalPort)
	if err != nil {
		return api.Session{}, err
	}

	now := time.Now().UTC()
	return m.storeSession(target, sessionParams{
		name:          fmt.Sprintf("%s-%d", target.Protocol, req.LocalPort),
		localPort:     req.LocalPort,
		status:        "connected",
		lastHeartbeat: &now,
		conn:          req.Conn,
		remoteAddr:    req.Conn.RemoteAddr().String(),
		authToken:     req.AuthToken,
		metadata:      req.Metadata,
		createdAt:     now,
	})
}

// TouchHeartbeat updates the last-seen heartbeat for a live session.
func (m *Manager) TouchHeartbeat(sessionID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	record, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session %q not found", sessionID)
	}

	timestamp := at.UTC()
	record.session.LastHeartbeat = &timestamp
	record.session.Status = "connected"
	return nil
}

// Remove forgets a session from the in-memory active store.
func (m *Manager) Remove(sessionID string) {
	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()
}

// List returns all known sessions ordered by creation time.
func (m *Manager) List(_ context.Context) []api.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]api.Session, 0, len(m.sessions))
	for _, record := range m.sessions {
		sessions = append(sessions, cloneSession(record.session))
	}

	slices.SortFunc(sessions, func(a, b api.Session) int {
		switch {
		case a.CreatedAt.Before(b.CreatedAt):
			return -1
		case a.CreatedAt.After(b.CreatedAt):
			return 1
		default:
			return strings.Compare(a.ID, b.ID)
		}
	})

	return sessions
}

type sessionParams struct {
	name          string
	localPort     int
	status        string
	lastHeartbeat *time.Time
	conn          net.Conn
	remoteAddr    string
	authToken     string
	metadata      api.ClientMetadata
	createdAt     time.Time
}

func (m *Manager) storeSession(target transport.Target, params sessionParams) (api.Session, error) {
	sessionID, err := newSessionID()
	if err != nil {
		return api.Session{}, err
	}

	route := m.planner.Plan(target, sessionID)
	descriptor := m.engine.Prepare(sessionID, params.name, target.Protocol, route)

	createdAt := params.createdAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	session := api.Session{
		ID:            sessionID,
		Name:          params.name,
		Protocol:      target.Protocol,
		Target:        target.URL.String(),
		LocalPort:     params.localPort,
		PublicURL:     descriptor.PublicURL,
		Status:        params.status,
		CreatedAt:     createdAt,
		LastHeartbeat: cloneTimePtr(params.lastHeartbeat),
		Route: api.Route{
			Upstream: route.Upstream,
			Headers:  route.Headers,
		},
	}

	m.mu.Lock()
	m.sessions[session.ID] = &record{
		session:    session,
		conn:       params.conn,
		remoteAddr: params.remoteAddr,
		authToken:  params.authToken,
		metadata:   params.metadata,
	}
	m.mu.Unlock()

	return cloneSession(session), nil
}

func cloneSession(in api.Session) api.Session {
	out := in
	out.LastHeartbeat = cloneTimePtr(in.LastHeartbeat)
	return out
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}

func inferLocalPort(target *url.URL) int {
	if target == nil {
		return 0
	}

	port := target.Port()
	if port == "" {
		return 0
	}

	value, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return value
}

func newSessionID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
