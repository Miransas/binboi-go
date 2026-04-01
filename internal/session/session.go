package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sardorazimov/binboi-go/internal/proxy"
	"github.com/sardorazimov/binboi-go/internal/transport"
	"github.com/sardorazimov/binboi-go/internal/tunnel"
	"github.com/sardorazimov/binboi-go/pkg/api"
)

// Manager stores session state and prepares public session metadata.
type Manager struct {
	mu       sync.RWMutex
	engine   *tunnel.Engine
	planner  *proxy.Planner
	sessions map[string]api.Session
}

// NewManager creates an in-memory session manager.
func NewManager(publicHost, forwardedHeader string) *Manager {
	return &Manager{
		engine:   tunnel.NewEngine(publicHost),
		planner:  proxy.NewPlanner(forwardedHeader),
		sessions: make(map[string]api.Session),
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

	sessionID, err := newSessionID()
	if err != nil {
		return api.Session{}, err
	}

	route := m.planner.Plan(target, sessionID)
	descriptor := m.engine.Prepare(sessionID, req.Name, target.Protocol, route)

	session := api.Session{
		ID:        sessionID,
		Name:      req.Name,
		Protocol:  target.Protocol,
		Target:    target.URL.String(),
		PublicURL: descriptor.PublicURL,
		Status:    "ready",
		CreatedAt: time.Now().UTC(),
		Route: api.Route{
			Upstream: route.Upstream,
			Headers:  route.Headers,
		},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	return session, nil
}

// List returns all known sessions ordered by creation time.
func (m *Manager) List(_ context.Context) []api.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]api.Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
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

func newSessionID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
