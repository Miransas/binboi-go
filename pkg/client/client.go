package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

// Client talks to the Binboi control-plane API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New builds a client for a control-plane base URL.
func New(baseURL string) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("base URL %q must include scheme and host", baseURL)
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

// CreateSessionInput is a convenience helper for examples and tests.
func CreateSessionInput(name, target string) api.CreateSessionRequest {
	return api.CreateSessionRequest{
		Name:   name,
		Target: target,
	}
}

// Health fetches the daemon health response.
func (c *Client) Health(ctx context.Context) (api.HealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return api.HealthResponse{}, err
	}

	var out api.HealthResponse
	if err := c.do(req, http.StatusOK, &out); err != nil {
		return api.HealthResponse{}, err
	}

	return out, nil
}

// CreateSession requests a new session from the daemon.
func (c *Client) CreateSession(ctx context.Context, input api.CreateSessionRequest) (api.Session, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return api.Session{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return api.Session{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	var out api.CreateSessionResponse
	if err := c.do(req, http.StatusCreated, &out); err != nil {
		return api.Session{}, err
	}

	return out.Session, nil
}

// ListSessions fetches known sessions from the daemon.
func (c *Client) ListSessions(ctx context.Context) ([]api.Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/sessions", nil)
	if err != nil {
		return nil, err
	}

	var out api.ListSessionsResponse
	if err := c.do(req, http.StatusOK, &out); err != nil {
		return nil, err
	}

	return out.Sessions, nil
}

func (c *Client) do(req *http.Request, expectedStatus int, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != expectedStatus {
		var apiErr api.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&apiErr); err == nil && apiErr.Error != "" {
			return fmt.Errorf("control API returned %d: %s", resp.StatusCode, apiErr.Error)
		}
		return fmt.Errorf("control API returned unexpected status %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	return nil
}
