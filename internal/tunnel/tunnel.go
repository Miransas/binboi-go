package tunnel

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/sardorazimov/binboi-go/internal/proxy"
)

// Descriptor captures the public tunnel information generated for a session.
type Descriptor struct {
	SessionID string
	Name      string
	PublicURL string
	Protocol  string
	Route     proxy.Route
}

// Engine builds stable tunnel descriptors from session and route metadata.
type Engine struct {
	publicHost string
}

// NewEngine creates a tunnel planner for a given public host.
func NewEngine(publicHost string) *Engine {
	return &Engine{publicHost: publicHost}
}

// Prepare builds a descriptor that other engine packages can consume.
func (e *Engine) Prepare(sessionID, name, protocol string, route proxy.Route) Descriptor {
	label := sanitizeLabel(name)
	if label == "" {
		label = sessionID
	}

	host := fmt.Sprintf("%s.%s", label, e.publicHost)
	publicURL := "http://" + host
	if protocol == "https" {
		publicURL = "https://" + host
	}
	if protocol == "tcp" {
		publicURL = "tcp://" + host
	}

	return Descriptor{
		SessionID: sessionID,
		Name:      name,
		PublicURL: publicURL,
		Protocol:  protocol,
		Route:     route,
	}
}

func sanitizeLabel(name string) string {
	var b strings.Builder
	prevDash := false

	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || unicode.IsSpace(r):
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}
