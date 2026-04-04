package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RemoteValidatorConfig controls frontend-backed token validation.
type RemoteValidatorConfig struct {
	SharedSecret string
	Timeout      time.Duration
	CacheTTL     time.Duration
}

type remoteValidateRequest struct {
	Token string `json:"token"`
}

type remoteValidateResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Data    struct {
		TokenID     string `json:"tokenId"`
		Prefix      string `json:"prefix"`
		PrincipalID string `json:"principalId"`
	} `json:"data"`
}

type remoteCacheEntry struct {
	principal Principal
	expiresAt time.Time
}

// RemoteValidator authenticates raw tokens against the frontend source-of-truth API.
type RemoteValidator struct {
	validateURL  string
	sharedSecret string
	httpClient   *http.Client
	logger       *slog.Logger
	cacheTTL     time.Duration

	mu    sync.Mutex
	cache map[string]remoteCacheEntry
}

// NewRemoteValidator builds a frontend-backed token validator.
func NewRemoteValidator(validateURL string, cfg RemoteValidatorConfig, logger *slog.Logger) *RemoteValidator {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 30 * time.Second
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(ioDiscard{}, nil))
	}

	return &RemoteValidator{
		validateURL:  strings.TrimSpace(validateURL),
		sharedSecret: strings.TrimSpace(cfg.SharedSecret),
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		logger:   logger,
		cacheTTL: cfg.CacheTTL,
		cache:    make(map[string]remoteCacheEntry),
	}
}

// Validate authenticates a raw token through the configured frontend endpoint.
func (v *RemoteValidator) Validate(ctx context.Context, rawToken string) (Principal, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return Principal{}, ErrTokenRequired
	}

	if _, err := PrefixFromRawToken(rawToken); err != nil {
		return Principal{}, err
	}

	hash := HashToken(rawToken)
	if principal, ok := v.cached(hash); ok {
		return principal, nil
	}

	payload, err := json.Marshal(remoteValidateRequest{Token: rawToken})
	if err != nil {
		return Principal{}, fmt.Errorf("marshal remote validation request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		v.validateURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("build remote validation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if v.sharedSecret != "" {
		req.Header.Set("x-binboi-internal-secret", v.sharedSecret)
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return Principal{}, fmt.Errorf("remote token validation failed: %w", err)
	}
	defer resp.Body.Close()

	var decoded remoteValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil && err != io.EOF {
		return Principal{}, fmt.Errorf("decode remote validation response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		principal := Principal{
			TokenID: decoded.Data.TokenID,
			UserID:  decoded.Data.PrincipalID,
			Prefix:  decoded.Data.Prefix,
		}
		v.cachePrincipal(hash, principal)
		return principal, nil
	case http.StatusBadRequest:
		return Principal{}, ErrTokenRequired
	case http.StatusForbidden:
		if decoded.Error == "revoked_token" {
			return Principal{}, ErrRevokedToken
		}
		return Principal{}, fmt.Errorf("remote validation forbidden: %s", decoded.Message)
	case http.StatusUnauthorized:
		if decoded.Error == "invalid_token" || decoded.Error == "token_required" || decoded.Error == "" {
			if decoded.Error == "token_required" {
				return Principal{}, ErrTokenRequired
			}
			return Principal{}, ErrInvalidToken
		}
		return Principal{}, fmt.Errorf("remote validation unauthorized: %s", decoded.Message)
	default:
		if decoded.Message != "" {
			return Principal{}, fmt.Errorf("remote validation returned %d: %s", resp.StatusCode, decoded.Message)
		}
		return Principal{}, fmt.Errorf("remote validation returned unexpected status %d", resp.StatusCode)
	}
}

func (v *RemoteValidator) cached(hash string) (Principal, bool) {
	now := time.Now().UTC()

	v.mu.Lock()
	defer v.mu.Unlock()

	entry, ok := v.cache[hash]
	if !ok {
		return Principal{}, false
	}
	if now.After(entry.expiresAt) {
		delete(v.cache, hash)
		return Principal{}, false
	}
	return entry.principal, true
}

func (v *RemoteValidator) cachePrincipal(hash string, principal Principal) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.cache[hash] = remoteCacheEntry{
		principal: principal,
		expiresAt: time.Now().UTC().Add(v.cacheTTL),
	}
}
