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
	tunnel      tunnel.Record
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
	mu                sync.RWMutex
	engine            *tunnel.Engine
	planner           *proxy.Planner
	store             *tunnel.Store
	sessions          map[string]*record
	byHost            map[string]string
	bySubdomain       map[string]string
	activeByHost      map[string]string
	activeBySubdomain map[string]string
	byResume          map[string]string
}

// NewManager creates an in-memory session manager.
func NewManager(publicHost, forwardedHeader string) *Manager {
	return NewManagerWithStore(publicHost, forwardedHeader, nil)
}

// NewManagerWithStore creates a session manager with optional durable tunnel storage.
func NewManagerWithStore(publicHost, forwardedHeader string, store *tunnel.Store) *Manager {
	return &Manager{
		engine:            tunnel.NewEngine(publicHost),
		planner:           proxy.NewPlanner(forwardedHeader),
		store:             store,
		sessions:          make(map[string]*record),
		byHost:            make(map[string]string),
		bySubdomain:       make(map[string]string),
		activeByHost:      make(map[string]string),
		activeBySubdomain: make(map[string]string),
		byResume:          make(map[string]string),
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
	record.session.TokenID = req.TokenID
	record.session.LastSeen = &now
	record.session.LastHeartbeat = &now
	record.tunnel.Status = "connected"
	record.tunnel.LastSeen = cloneTimePtr(&now)
	m.activateRoutingLocked(record.session.ID, record.session.PublicURL, record.session.Subdomain)

	_ = m.persistTunnelLocked(record)
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
	record.tunnel.Status = "disconnected"
	record.tunnel.LastSeen = cloneTimePtr(&timestamp)
	m.deactivateRoutingLocked(record.session.ID, record.session.PublicURL, record.session.Subdomain)
	_ = m.persistTunnelLocked(record)
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
	record.tunnel.Status = "connected"
	record.tunnel.LastSeen = cloneTimePtr(&timestamp)
	m.activateRoutingLocked(record.session.ID, record.session.PublicURL, record.session.Subdomain)
	_ = m.persistTunnelLocked(record)
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

	sessionID, ok := m.activeByHost[host]
	if !ok {
		subdomain := tunnel.ExtractSubdomain(host, m.engine.PublicHost())
		if subdomain != "" {
			sessionID, ok = m.activeBySubdomain[subdomain]
		}
	}
	if !ok {
		sessionID, ok = m.byHost[host]
	}
	if !ok {
		subdomain := tunnel.ExtractSubdomain(host, m.engine.PublicHost())
		if subdomain != "" {
			sessionID, ok = m.bySubdomain[subdomain]
		}
	}
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
	subdomain     string
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
	subdomain, err := m.allocateSubdomain(params.subdomain)
	if err != nil {
		return api.Session{}, err
	}
	descriptor := m.engine.Prepare(sessionID, params.name, subdomain, target.Protocol, route)

	createdAt := params.createdAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	session := api.Session{
		ID:            sessionID,
		Name:          params.name,
		UserID:        params.userID,
		TokenID:       params.tokenID,
		Subdomain:     descriptor.Subdomain,
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
			m.removeSessionLocked(existingID, existing)
		}
	}

	record := &record{
		session: session,
		tunnel: tunnel.Record{
			ID:        session.ID,
			UserID:    params.userID,
			TokenID:   params.tokenID,
			Subdomain: descriptor.Subdomain,
			Protocol:  target.Protocol,
			Port:      params.localPort,
			Status:    params.status,
			PublicURL: descriptor.PublicURL,
			CreatedAt: createdAt,
			LastSeen:  cloneTimePtr(params.lastSeen),
		},
		conn:        params.conn,
		remoteAddr:  params.remoteAddr,
		userID:      params.userID,
		tokenID:     params.tokenID,
		tokenPrefix: params.tokenPrefix,
		metadata:    params.metadata,
		resumeToken: resumeToken,
	}
	m.sessions[session.ID] = record
	if host != "" {
		m.byHost[host] = session.ID
	}
	if descriptor.Subdomain != "" {
		m.bySubdomain[descriptor.Subdomain] = session.ID
	}
	if session.Connection == "connected" {
		m.activateRoutingLocked(session.ID, session.PublicURL, session.Subdomain)
	}
	m.byResume[resumeToken] = session.ID
	if err := m.persistTunnelLocked(record); err != nil {
		m.removeSessionLocked(session.ID, record)
		return api.Session{}, err
	}

	return cloneSession(session), nil
}

func (m *Manager) removeSessionLocked(sessionID string, record *record) {
	delete(m.byResume, record.resumeToken)
	delete(m.byHost, hostFromPublicURL(record.session.PublicURL))
	delete(m.bySubdomain, record.session.Subdomain)
	m.deactivateRoutingLocked(sessionID, record.session.PublicURL, record.session.Subdomain)
	delete(m.sessions, sessionID)
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

func (m *Manager) allocateSubdomain(requested string) (string, error) {
	requested = strings.TrimSpace(strings.ToLower(requested))
	if requested != "" {
		if err := m.ensureSubdomainAvailable(requested); err != nil {
			return "", err
		}
		return requested, nil
	}

	for attempt := 0; attempt < 32; attempt++ {
		subdomain, err := tunnel.GenerateSubdomain()
		if err != nil {
			return "", fmt.Errorf("generate subdomain: %w", err)
		}
		if err := m.ensureSubdomainAvailable(subdomain); err == nil {
			return subdomain, nil
		}
	}
	return "", errors.New("exhausted subdomain generation attempts")
}

func (m *Manager) ensureSubdomainAvailable(subdomain string) error {
	subdomain = strings.TrimSpace(strings.ToLower(subdomain))
	if subdomain == "" {
		return errors.New("subdomain cannot be empty")
	}

	m.mu.RLock()
	_, exists := m.bySubdomain[subdomain]
	m.mu.RUnlock()
	if exists {
		return fmt.Errorf("subdomain %q is already assigned", subdomain)
	}

	if m.store != nil {
		record, ok, err := m.store.FindBySubdomain(subdomain)
		if err != nil {
			return err
		}
		if ok && record.ID != "" {
			return fmt.Errorf("subdomain %q is already assigned", subdomain)
		}
	}
	return nil
}

func (m *Manager) activateRoutingLocked(sessionID, publicURL, subdomain string) {
	host := hostFromPublicURL(publicURL)
	if host != "" {
		m.activeByHost[host] = sessionID
	}
	if subdomain != "" {
		m.activeBySubdomain[subdomain] = sessionID
	}
}

func (m *Manager) deactivateRoutingLocked(sessionID, publicURL, subdomain string) {
	host := hostFromPublicURL(publicURL)
	if host != "" {
		if current, ok := m.activeByHost[host]; ok && current == sessionID {
			delete(m.activeByHost, host)
		}
	}
	if subdomain != "" {
		if current, ok := m.activeBySubdomain[subdomain]; ok && current == sessionID {
			delete(m.activeBySubdomain, subdomain)
		}
	}
}

func (m *Manager) persistTunnelLocked(record *record) error {
	if m.store == nil || record == nil {
		return nil
	}
	if err := m.store.Upsert(record.tunnel); err != nil {
		return fmt.Errorf("persist tunnel %s: %w", record.tunnel.ID, err)
	}
	return nil
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
