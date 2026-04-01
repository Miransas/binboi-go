package transport

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

// Target is the normalized representation of an upstream address.
type Target struct {
	Raw      string
	URL      *url.URL
	Protocol string
}

// ParseTarget validates a raw target and infers its protocol.
func ParseTarget(raw, explicitProtocol string) (Target, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Target{}, fmt.Errorf("parse target: %w", err)
	}
	if parsed.Scheme == "" {
		return Target{}, fmt.Errorf("target %q must include a scheme such as http:// or tcp://", raw)
	}
	if parsed.Host == "" {
		return Target{}, fmt.Errorf("target %q must include a host", raw)
	}

	protocol := strings.ToLower(strings.TrimSpace(explicitProtocol))
	if protocol == "" {
		protocol = inferProtocol(parsed.Scheme)
	}

	if _, err := api.NormalizeTunnelProtocol(protocol); err != nil {
		return Target{}, err
	}

	return Target{
		Raw:      raw,
		URL:      parsed,
		Protocol: protocol,
	}, nil
}

// LocalTarget builds a loopback target for a locally running service.
func LocalTarget(protocol string, port int) (Target, error) {
	normalized, err := api.NormalizeTunnelProtocol(protocol)
	if err != nil {
		return Target{}, err
	}
	if port < 1 || port > 65535 {
		return Target{}, fmt.Errorf("local port %d must be between 1 and 65535", port)
	}

	raw := fmt.Sprintf("%s://127.0.0.1:%d", normalized, port)
	return ParseTarget(raw, normalized)
}

func inferProtocol(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "https"
	case "tcp":
		return "tcp"
	default:
		return "http"
	}
}
