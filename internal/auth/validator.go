package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ValidatorConfig tunes cache and persistence behavior for token checks.
type ValidatorConfig struct {
	CacheTTL               time.Duration
	LastUsedUpdateInterval time.Duration
}

type cacheEntry struct {
	principal          Principal
	expiresAt          time.Time
	lastPersistedUsage time.Time
}

// Validator authenticates raw tokens against the durable store and cache.
type Validator struct {
	store      *Store
	logger     *slog.Logger
	cacheTTL   time.Duration
	touchEvery time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// NewValidator builds a token validator.
func NewValidator(store *Store, cfg ValidatorConfig, logger *slog.Logger) *Validator {
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 30 * time.Second
	}
	if cfg.LastUsedUpdateInterval <= 0 {
		cfg.LastUsedUpdateInterval = time.Minute
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(ioDiscard{}, nil))
	}

	return &Validator{
		store:      store,
		logger:     logger,
		cacheTTL:   cfg.CacheTTL,
		touchEvery: cfg.LastUsedUpdateInterval,
		cache:      make(map[string]cacheEntry),
	}
}

// Validate authenticates a raw token and returns its principal.
func (v *Validator) Validate(ctx context.Context, rawToken string) (Principal, error) {
	_ = ctx

	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return Principal{}, ErrTokenRequired
	}

	prefix, err := PrefixFromRawToken(rawToken)
	if err != nil {
		return Principal{}, err
	}

	hash := HashToken(rawToken)
	if principal, ok := v.cached(hash); ok {
		v.touchIfNeeded(principal.TokenID)
		return principal, nil
	}

	record, ok, err := v.store.FindByPrefix(prefix)
	if err != nil {
		return Principal{}, err
	}
	if !ok {
		return Principal{}, ErrInvalidToken
	}
	if record.Revoked {
		return Principal{}, ErrRevokedToken
	}
	if !CompareHashConstantTime(record.KeyHash, hash) {
		return Principal{}, ErrInvalidToken
	}

	principal := Principal{
		TokenID: record.ID,
		UserID:  record.UserID,
		Prefix:  record.Prefix,
	}
	now := time.Now().UTC()
	if err := v.store.TouchLastUsed(record.ID, now); err != nil {
		return Principal{}, fmt.Errorf("touch last_used_at: %w", err)
	}

	v.mu.Lock()
	v.cache[hash] = cacheEntry{
		principal:          principal,
		expiresAt:          now.Add(v.cacheTTL),
		lastPersistedUsage: now,
	}
	v.mu.Unlock()

	return principal, nil
}

func (v *Validator) cached(hash string) (Principal, bool) {
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

func (v *Validator) touchIfNeeded(tokenID string) {
	now := time.Now().UTC()

	v.mu.Lock()
	var (
		hashToUpdate string
		entry        cacheEntry
		found        bool
	)
	for hash, candidate := range v.cache {
		if candidate.principal.TokenID == tokenID {
			hashToUpdate = hash
			entry = candidate
			found = true
			break
		}
	}
	if !found || now.Sub(entry.lastPersistedUsage) < v.touchEvery {
		v.mu.Unlock()
		return
	}
	entry.lastPersistedUsage = now
	entry.expiresAt = now.Add(v.cacheTTL)
	v.cache[hashToUpdate] = entry
	v.mu.Unlock()

	if err := v.store.TouchLastUsed(tokenID, now); err != nil {
		v.logger.Warn("failed to persist token last_used_at from cache",
			"token_id", tokenID,
			"error", err,
		)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
