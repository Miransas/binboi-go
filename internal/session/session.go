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

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrInvalidResume   = errors.New("invalid resume")
)

// RegisterRequest captures the information needed to create or resume a control session.
type RegisterRequest struct {
	Protocol       string
	LocalPort      int
	UserID         string
	TokenID        string
	TokenPrefix    string
	Metadata       api.ClientMetadata
	Conn           net.Conn
	ResumeTunnelID string
	ResumeToken    string
}

// ResumeResult describes whether a registration attached to an existing session.
type ResumeResult struct {
	Session api.Session
	Resumed bool
}

type record struct {
	session     api.Session
	conn        net.Conn
	remoteAddr  string
	userID      string
	tokenID     string
	tokenPrefix string
	metadata    api.ClientMetadata
	resumeToken string
}

// Manager stores session state and prepares public session metadata.
type Manager struct {
	mu       sync.RWMutex
	engine   *tunnel.Engine
	planner  *proxy.Planner
	sessions map[string]*record
	byHost   map[string]string
	byResume map[string]string
}

// NewManager creates an in-memory session manager.
func NewManager(publicHost, forwardedHeader string) *Manager {
	return &Manager{
		engine:   tunnel.NewEngine(publicHost),
		planner:  proxy.NewPlanner(forwardedHeader),
		sessions: make(map[string]*record),
		byHost:   make(map[string]string),
		byResume: make(map[string]string),
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
		name:       req.Name,
		localPort:  inferLocalPort(target.URL),
		status:     "ready",
		connection: "idle",
	})
}

// RegisterTunnel validates a stream registration request and creates or resumes a session.
func (m *Manager) RegisterTunnel(_ context.Context, req RegisterRequest) (ResumeResult, error) {
	if req.Conn == nil {
		return ResumeResult{}, errors.New("register request requires a live control connection")
	}
	if _, err := api.NormalizeTunnelProtocol(req.Protocol); err != nil {
		return ResumeResult{}, err
	}

	if req.ResumeTunnelID != "" || req.ResumeToken != "" {
		session, err := m.resumeTunnel(req)
		if err != nil {
			return ResumeResult{}, err
		}
		return ResumeResult{Session: session, Resumed: true}, nil
	}

	target, err := transport.LocalTarget(req.Protocol, req.LocalPort)
	if err != nil {
		return ResumeResult{}, err
	}

	now := time.Now().UTC()
	session, err := m.storeSession(target, sessionParams{
		name:          fmt.Sprintf("%s-%d", target.Protocol, req.LocalPort),
		localPort:     req.LocalPort,
		status:        "connected",
		connection:    "connected",
		lastHeartbeat: &now,
		lastSeen:      &now,
		conn:          req.Conn,
		remoteAddr:    req.Conn.RemoteAddr().String(),
		userID:        req.UserID,
		tokenID:       req.TokenID,
		tokenPrefix:   req.TokenPrefix,
		metadata:      req.Metadata,
		createdAt:     now,
	})
	if err != nil {
		return ResumeResult{}, err
	}

	return ResumeResult{Session: session}, nil
}

func (m *Manager) resumeTunnel(req RegisterRequest) (api.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	record, ok := m.sessions[req.ResumeTunnelID]
	if !ok {
		return api.Session{}, ErrInvalidResume
	}
	if record.resumeToken == "" || record.resumeToken != req.ResumeToken {
		return api.Session{}, ErrInvalidResume
	}
	if record.session.Protocol != req.Protocol || record.session.LocalPort != req.LocalPort {
		return api.Session{}, ErrInvalidResume
	}
	if record.tokenID != "" || req.TokenID != "" || record.userID != "" || req.UserID != "" {
		if record.tokenID != req.TokenID || record.userID != req.UserID {
			return api.Session{}, ErrInvalidResume
		}
	}

	now := time.Now().UTC()
	record.conn = req.Conn
	record.remoteAddr = req.Conn.RemoteAddr().String()
	record.userID = req.UserID
	record.tokenID = req.TokenID
	record.tokenPrefix = req.TokenPrefix
	record.metadata = req.Metadata
	record.session.Status = "connected"
	record.session.Connection = "connected"
	record.session.UserID = req.UserID
	record.session.LastSeen = &now
	record.session.LastHeartbeat = &now

	return cloneSession(record.session), nil
}

// DetachConnection marks a session as disconnected but keeps it resumable.
func (m *Manager) DetachConnection(sessionID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	record, ok := m.sessions[sessionID]
	if !ok {
		return
	}

	record.conn = nil
	record.remoteAddr = ""
	timestamp := at.UTC()
	record.session.Connection = "disconnected"
	record.session.Status = "disconnected"
	record.session.LastSeen = &timestamp
	record.session.InFlight = 0
}

// TouchHeartbeat updates the last-seen heartbeat for a live session.
func (m *Manager) TouchHeartbeat(sessionID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	record, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}

	timestamp := at.UTC()
	record.session.LastHeartbeat = &timestamp
	record.session.LastSeen = &timestamp
	record.session.Status = "connected"
	record.session.Connection = "connected"
	return nil
}

// SetInFlight updates the in-flight request count for a session.
func (m *Manager) SetInFlight(sessionID string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	record, ok := m.sessions[sessionID]
	if !ok {
		return
	}
	record.session.InFlight = count
}

// LookupByHost returns the session currently owning a public host.
func (m *Manager) LookupByHost(host string) (api.Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessionID, ok := m.byHost[host]
	if !ok {
		return api.Session{}, false
	}
	record, ok := m.sessions[sessionID]
	if !ok {
		return api.Session{}, false
	}
	return cloneSession(record.session), true
}

// ResumeToken returns the resume token for a session.
func (m *Manager) ResumeToken(sessionID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	record, ok := m.sessions[sessionID]
	if !ok {
		return "", false
	}
	return record.resumeToken, true
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
	connection    string
	lastHeartbeat *time.Time
	lastSeen      *time.Time
	conn          net.Conn
	remoteAddr    string
	userID        string
	tokenID       string
	tokenPrefix   string
	metadata      api.ClientMetadata
	createdAt     time.Time
}

func (m *Manager) storeSession(target transport.Target, params sessionParams) (api.Session, error) {
	sessionID, err := newSessionID()
	if err != nil {
		return api.Session{}, err
	}
	resumeToken, err := newResumeToken()
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
		UserID:        params.userID,
		Protocol:      target.Protocol,
		Target:        target.URL.String(),
		LocalPort:     params.localPort,
		PublicURL:     descriptor.PublicURL,
		Status:        params.status,
		Connection:    params.connection,
		CreatedAt:     createdAt,
		LastSeen:      cloneTimePtr(params.lastSeen),
		LastHeartbeat: cloneTimePtr(params.lastHeartbeat),
		Route: api.Route{
			Upstream: route.Upstream,
			Headers:  route.Headers,
		},
	}

	host := hostFromPublicURL(session.PublicURL)

	m.mu.Lock()
	defer m.mu.Unlock()

	if existingID, ok := m.byHost[host]; ok {
		if existing, exists := m.sessions[existingID]; exists {
			if existing.session.Connection == "connected" {
				return api.Session{}, fmt.Errorf("public host %q is already connected", host)
			}
			delete(m.byResume, existing.resumeToken)
			delete(m.sessions, existingID)
		}
	}

	m.sessions[session.ID] = &record{
		session:     session,
		conn:        params.conn,
		remoteAddr:  params.remoteAddr,
		userID:      params.userID,
		tokenID:     params.tokenID,
		tokenPrefix: params.tokenPrefix,
		metadata:    params.metadata,
		resumeToken: resumeToken,
	}
	if host != "" {
		m.byHost[host] = session.ID
	}
	m.byResume[resumeToken] = session.ID

	return cloneSession(session), nil
}

func cloneSession(in api.Session) api.Session {
	out := in
	out.LastSeen = cloneTimePtr(in.LastSeen)
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

func hostFromPublicURL(publicURL string) string {
	parsed, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
	return host
}

func newSessionID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func newResumeToken() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
