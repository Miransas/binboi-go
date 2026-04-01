package proxy

import "github.com/sardorazimov/binboi-go/internal/transport"

// Route describes how a session should reach its upstream target.
type Route struct {
	Upstream string            `json:"upstream"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// Planner builds route descriptions from normalized transport targets.
type Planner struct {
	forwardedHeader string
}

// NewPlanner returns a route planner with basic header propagation defaults.
func NewPlanner(forwardedHeader string) *Planner {
	return &Planner{forwardedHeader: forwardedHeader}
}

// Plan converts a target into lightweight route metadata.
func (p *Planner) Plan(target transport.Target, sessionID string) Route {
	headers := map[string]string{}
	if p.forwardedHeader != "" {
		headers[p.forwardedHeader] = sessionID
	}

	return Route{
		Upstream: target.URL.String(),
		Headers:  headers,
	}
}
