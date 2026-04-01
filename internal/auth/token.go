package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	tokenSchemaVersion   = 1
	tokenPrefixPrefix    = "bin_live"
	tokenSecretBytes     = 24
	tokenPrefixSuffixLen = 4
)

var (
	ErrTokenRequired = errors.New("auth token is required")
	ErrInvalidToken  = errors.New("invalid auth token")
	ErrRevokedToken  = errors.New("auth token has been revoked")
)

// Record is the persisted token model.
type Record struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	KeyHash    string     `json:"key_hash"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Revoked    bool       `json:"revoked"`
}

// Database is the versioned on-disk token database schema.
type Database struct {
	SchemaVersion int      `json:"schema_version"`
	Tokens        []Record `json:"tokens"`
}

// PublicToken is the non-secret view of a token.
type PublicToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Revoked    bool       `json:"revoked"`
}

// CreateResult returns the public token record plus the raw token exactly once.
type CreateResult struct {
	Token    PublicToken `json:"token"`
	RawToken string      `json:"raw_token"`
}

// Principal describes the validated owner of a token.
type Principal struct {
	TokenID string
	UserID  string
	Prefix  string
}

// Public converts a persisted record to its non-secret representation.
func (r Record) Public() PublicToken {
	return PublicToken{
		ID:         r.ID,
		UserID:     r.UserID,
		Prefix:     r.Prefix,
		CreatedAt:  r.CreatedAt,
		LastUsedAt: cloneTimePtr(r.LastUsedAt),
		Revoked:    r.Revoked,
	}
}

// EmptyDatabase returns a new schema-compatible token database.
func EmptyDatabase() Database {
	return Database{
		SchemaVersion: tokenSchemaVersion,
		Tokens:        []Record{},
	}
}

// GenerateToken creates a new token record and raw token for a user.
func GenerateToken(_ context.Context, userID string, now time.Time) (Record, string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return Record{}, "", errors.New("user_id is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return Record{}, "", fmt.Errorf("generate token id: %w", err)
	}

	prefixSuffixBytes := make([]byte, tokenPrefixSuffixLen)
	if _, err := rand.Read(prefixSuffixBytes); err != nil {
		return Record{}, "", fmt.Errorf("generate token prefix: %w", err)
	}

	secretBytes := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return Record{}, "", fmt.Errorf("generate token secret: %w", err)
	}

	prefix := fmt.Sprintf("%s_%s", tokenPrefixPrefix, strings.ToLower(hex.EncodeToString(prefixSuffixBytes)))
	secret := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secretBytes))
	rawToken := prefix + "_" + secret
	keyHash := HashToken(rawToken)

	record := Record{
		ID:        hex.EncodeToString(idBytes),
		UserID:    userID,
		KeyHash:   keyHash,
		Prefix:    prefix,
		CreatedAt: now.UTC(),
		Revoked:   false,
	}
	return record, rawToken, nil
}

// HashToken computes the hash stored for a raw token.
func HashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawToken)))
	return hex.EncodeToString(sum[:])
}

// PrefixFromRawToken extracts the human-readable prefix from a raw token.
func PrefixFromRawToken(rawToken string) (string, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return "", ErrTokenRequired
	}

	lastUnderscore := strings.LastIndex(rawToken, "_")
	if lastUnderscore <= 0 || lastUnderscore == len(rawToken)-1 {
		return "", ErrInvalidToken
	}

	prefix := rawToken[:lastUnderscore]
	if !strings.HasPrefix(prefix, tokenPrefixPrefix+"_") {
		return "", ErrInvalidToken
	}
	return prefix, nil
}

// CompareHashConstantTime compares a computed token hash with the stored hash.
func CompareHashConstantTime(storedHash, computedHash string) bool {
	if len(storedHash) != len(computedHash) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(storedHash), []byte(computedHash)) == 1
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}
