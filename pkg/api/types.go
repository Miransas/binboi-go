package api

import "time"

// CreateSessionRequest asks the daemon to register a new session.
type CreateSessionRequest struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	Protocol string `json:"protocol,omitempty"`
	Token    string `json:"token,omitempty"`
}

// CreateSessionResponse returns the newly created session.
type CreateSessionResponse struct {
	Session Session `json:"session"`
}

// ListSessionsResponse returns all known sessions.
type ListSessionsResponse struct {
	Sessions []Session `json:"sessions"`
}

// Session describes a Binboi session managed by the daemon.
type Session struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Protocol      string     `json:"protocol"`
	Target        string     `json:"target"`
	LocalPort     int        `json:"local_port,omitempty"`
	PublicURL     string     `json:"public_url"`
	Status        string     `json:"status"`
	Connection    string     `json:"connection"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeen      *time.Time `json:"last_seen,omitempty"`
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
	InFlight      int        `json:"in_flight,omitempty"`
	Route         Route      `json:"route"`
}

// Route describes the upstream route selected for a session.
type Route struct {
	Upstream string            `json:"upstream"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// HealthResponse reports daemon liveness.
type HealthResponse struct {
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

// ErrorResponse represents a JSON API error.
type ErrorResponse struct {
	Error string `json:"error"`
}
