package tunnel

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"

	"github.com/sardorazimov/binboi-go/internal/proxy"
)

const randomSubdomainBytes = 5

// Descriptor captures the public tunnel information generated for a session.
type Descriptor struct {
	SessionID string
	Name      string
	Subdomain string
	Host      string
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

// PublicHost returns the configured public suffix for generated routes.
func (e *Engine) PublicHost() string {
	return e.publicHost
}

// HostForSubdomain builds the fully qualified public host for a subdomain.
func (e *Engine) HostForSubdomain(subdomain string) string {
	subdomain = sanitizeLabel(subdomain)
	if subdomain == "" {
		return e.publicHost
	}
	if e.publicHost == "" {
		return subdomain
	}
	return fmt.Sprintf("%s.%s", subdomain, e.publicHost)
}

// PublicURLForSubdomain builds the public URL for a protocol and subdomain.
func (e *Engine) PublicURLForSubdomain(subdomain, protocol string) string {
	host := e.HostForSubdomain(subdomain)
	publicURL := "http://" + host
	if protocol == "https" {
		publicURL = "https://" + host
	}
	if protocol == "tcp" {
		publicURL = "tcp://" + host
	}
	return publicURL
}

// Prepare builds a descriptor that other engine packages can consume.
func (e *Engine) Prepare(sessionID, name, subdomain, protocol string, route proxy.Route) Descriptor {
	subdomain = sanitizeLabel(subdomain)
	if subdomain == "" {
		subdomain = sessionID
	}

	host := e.HostForSubdomain(subdomain)
	publicURL := e.PublicURLForSubdomain(subdomain, protocol)

	return Descriptor{
		SessionID: sessionID,
		Name:      name,
		Subdomain: subdomain,
		Host:      host,
		PublicURL: publicURL,
		Protocol:  protocol,
		Route:     route,
	}
}

// GenerateSubdomain returns a DNS-safe random subdomain label.
func GenerateSubdomain() (string, error) {
	buf := make([]byte, randomSubdomainBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(hex.EncodeToString(buf)), nil
}

// ExtractSubdomain returns the leading subdomain for a configured public host.
func ExtractSubdomain(host, publicHost string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	publicHost = strings.TrimSpace(strings.ToLower(publicHost))
	if host == "" || publicHost == "" || host == publicHost {
		return ""
	}

	suffix := "." + publicHost
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	subdomain := strings.TrimSuffix(host, suffix)
	if subdomain == "" || strings.Contains(subdomain, ".") {
		return ""
	}
	return sanitizeLabel(subdomain)
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
