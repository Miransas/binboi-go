package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const tunnelSchemaVersion = 1

// Record is the durable tunnel model for routing and audit purposes.
type Record struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id,omitempty"`
	TokenID   string     `json:"token_id,omitempty"`
	Subdomain string     `json:"subdomain"`
	Protocol  string     `json:"protocol"`
	Port      int        `json:"port"`
	Status    string     `json:"status"`
	PublicURL string     `json:"public_url"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
}

// Database is the versioned on-disk tunnel schema.
type Database struct {
	SchemaVersion int      `json:"schema_version"`
	Tunnels       []Record `json:"tunnels"`
}

// Store persists tunnel records to a local versioned JSON database.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore constructs a tunnel store for a local file path.
func NewStore(path string) *Store {
	return &Store{path: filepath.Clean(path)}
}

// Path returns the configured tunnel database path.
func (s *Store) Path() string {
	return s.path
}

// Ensure creates the tunnel database file if it does not already exist.
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
	return s.saveLocked(Database{SchemaVersion: tunnelSchemaVersion, Tunnels: []Record{}})
}

// Upsert creates or replaces a durable tunnel record.
func (s *Store) Upsert(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		db = Database{SchemaVersion: tunnelSchemaVersion, Tunnels: []Record{}}
	}

	replaced := false
	for i := range db.Tunnels {
		if db.Tunnels[i].ID != record.ID {
			continue
		}
		db.Tunnels[i] = record
		replaced = true
		break
	}
	if !replaced {
		db.Tunnels = append(db.Tunnels, record)
	}

	return s.saveLocked(db)
}

// UpdateStatus updates an existing tunnel's status and last-seen timestamp.
func (s *Store) UpdateStatus(id, status string, lastSeen *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil {
		return err
	}

	for i := range db.Tunnels {
		if db.Tunnels[i].ID != id {
			continue
		}
		db.Tunnels[i].Status = status
		db.Tunnels[i].LastSeen = cloneTimePtr(lastSeen)
		return s.saveLocked(db)
	}

	return fmt.Errorf("tunnel %s not found", id)
}

// FindBySubdomain returns the durable tunnel record for a subdomain.
func (s *Store) FindBySubdomain(subdomain string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}

	for _, record := range db.Tunnels {
		if record.Subdomain == subdomain {
			return record, true, nil
		}
	}
	return Record{}, false, nil
}

// List returns all known tunnel records ordered by creation time.
func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Record{}, nil
		}
		return nil, err
	}

	records := append([]Record(nil), db.Tunnels...)
	slices.SortFunc(records, func(a, b Record) int {
		switch {
		case a.CreatedAt.Before(b.CreatedAt):
			return -1
		case a.CreatedAt.After(b.CreatedAt):
			return 1
		default:
			return 0
		}
	})
	return records, nil
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
		return Database{}, fmt.Errorf("decode tunnel database: %w", err)
	}
	if db.SchemaVersion == 0 {
		db.SchemaVersion = tunnelSchemaVersion
	}
	if db.SchemaVersion != tunnelSchemaVersion {
		return Database{}, fmt.Errorf("unsupported tunnel database schema version %d", db.SchemaVersion)
	}
	if db.Tunnels == nil {
		db.Tunnels = []Record{}
	}
	return db, nil
}

func (s *Store) saveLocked(db Database) error {
	if s.path == "" {
		return errors.New("tunnel database path cannot be empty")
	}
	db.SchemaVersion = tunnelSchemaVersion
	if db.Tunnels == nil {
		db.Tunnels = []Record{}
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create tunnel database directory: %w", err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(s.path), "binboi-tunnels-*.json")
	if err != nil {
		return fmt.Errorf("create tunnel database temp file: %w", err)
	}
	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("chmod tunnel database temp file: %w", err)
	}

	enc := json.NewEncoder(tempFile)
	enc.SetIndent("", "  ")
	if err := enc.Encode(db); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("write tunnel database: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("close tunnel database temp file: %w", err)
	}
	if err := os.Rename(tempFile.Name(), s.path); err != nil {
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("replace tunnel database: %w", err)
	}
	return nil
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}
