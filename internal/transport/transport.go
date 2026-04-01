package transport

import (
	"fmt"
	"net/url"
	"strings"
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

	switch protocol {
	case "http", "https", "tcp":
	default:
		return Target{}, fmt.Errorf("unsupported protocol %q", protocol)
	}

	return Target{
		Raw:      raw,
		URL:      parsed,
		Protocol: protocol,
	}, nil
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
