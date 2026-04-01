package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Store persists token records to a small versioned on-disk database file.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore constructs a file-backed token store.
func NewStore(path string) *Store {
	return &Store{path: filepath.Clean(path)}
}

// Path returns the configured database path.
func (s *Store) Path() string {
	return s.path
}

// Ensure creates the database file if it does not exist yet.
func (s *Store) Ensure() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.loadLocked()
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.saveLocked(EmptyDatabase())
}

// CreateToken generates and persists a new token for the given user.
func (s *Store) CreateToken(ctx context.Context, userID string) (CreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return CreateResult{}, err
	}
	if errors.Is(err, os.ErrNotExist) {
		db = EmptyDatabase()
	}

	record, rawToken, err := GenerateToken(ctx, userID, time.Now().UTC())
	if err != nil {
		return CreateResult{}, err
	}
	db.Tokens = append(db.Tokens, record)
	if err := s.saveLocked(db); err != nil {
		return CreateResult{}, err
	}

	return CreateResult{
		Token:    record.Public(),
		RawToken: rawToken,
	}, nil
}

// ListTokens returns the non-secret token list.
func (s *Store) ListTokens() ([]PublicToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []PublicToken{}, nil
		}
		return nil, err
	}

	tokens := make([]PublicToken, 0, len(db.Tokens))
	for _, token := range db.Tokens {
		tokens = append(tokens, token.Public())
	}
	slices.SortFunc(tokens, func(a, b PublicToken) int {
		switch {
		case a.CreatedAt.Before(b.CreatedAt):
			return -1
		case a.CreatedAt.After(b.CreatedAt):
			return 1
		default:
			return 0
		}
	})
	return tokens, nil
}

// RevokeToken marks a token as revoked.
func (s *Store) RevokeToken(tokenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil {
		return err
	}

	for i := range db.Tokens {
		if db.Tokens[i].ID != tokenID {
			continue
		}
		db.Tokens[i].Revoked = true
		return s.saveLocked(db)
	}
	return fmt.Errorf("token %s not found", tokenID)
}

// FindByPrefix returns the persisted token record for a given prefix.
func (s *Store) FindByPrefix(prefix string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}

	for _, token := range db.Tokens {
		if token.Prefix == prefix {
			return token, true, nil
		}
	}
	return Record{}, false, nil
}

// TouchLastUsed updates the token's last successful usage timestamp.
func (s *Store) TouchLastUsed(tokenID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil {
		return err
	}

	timestamp := at.UTC()
	for i := range db.Tokens {
		if db.Tokens[i].ID != tokenID {
			continue
		}
		db.Tokens[i].LastUsedAt = &timestamp
		return s.saveLocked(db)
	}
	return fmt.Errorf("token %s not found", tokenID)
}

func (s *Store) loadLocked() (Database, error) {
	file, err := os.Open(s.path)
	if err != nil {
		return Database{}, err
	}
	defer file.Close()

	var db Database
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&db); err != nil {
		return Database{}, fmt.Errorf("decode token database: %w", err)
	}
	if db.SchemaVersion == 0 {
		db.SchemaVersion = tokenSchemaVersion
	}
	if db.SchemaVersion != tokenSchemaVersion {
		return Database{}, fmt.Errorf("unsupported token database schema version %d", db.SchemaVersion)
	}
	if db.Tokens == nil {
		db.Tokens = []Record{}
	}
	return db, nil
}

func (s *Store) saveLocked(db Database) error {
	if s.path == "" {
		return errors.New("token database path cannot be empty")
	}
	db.SchemaVersion = tokenSchemaVersion
	if db.Tokens == nil {
		db.Tokens = []Record{}
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create token database directory: %w", err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(s.path), "binboi-tokens-*.json")
	if err != nil {
		return fmt.Errorf("create token database temp file: %w", err)
	}
	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("chmod token database temp file: %w", err)
	}

	enc := json.NewEncoder(tempFile)
	enc.SetIndent("", "  ")
	if err := enc.Encode(db); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("write token database: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("close token database temp file: %w", err)
	}
	if err := os.Rename(tempFile.Name(), s.path); err != nil {
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("replace token database: %w", err)
	}
	return nil
}
