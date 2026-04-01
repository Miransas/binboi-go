package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Store persists usage snapshots to a versioned JSON database.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore constructs a file-backed usage store.
func NewStore(path string) *Store {
	return &Store{path: filepath.Clean(path)}
}

// Path returns the configured usage database path.
func (s *Store) Path() string {
	return s.path
}

// Ensure creates the usage database file when it does not exist yet.
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
	return s.saveLocked(Database{SchemaVersion: usageSchemaVersion, Records: []Record{}})
}

// UpsertMany creates or updates usage snapshots in one file write.
func (s *Store) UpsertMany(records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.loadLocked()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		db = Database{SchemaVersion: usageSchemaVersion, Records: []Record{}}
	}

	for _, record := range records {
		replaced := false
		for i := range db.Records {
			if db.Records[i].UserID != record.UserID || db.Records[i].Period != record.Period || !db.Records[i].PeriodStart.Equal(record.PeriodStart) {
				continue
			}
			db.Records[i] = record
			replaced = true
			break
		}
		if !replaced {
			db.Records = append(db.Records, record)
		}
	}

	return s.saveLocked(db)
}

// List returns all known usage records sorted by time then user.
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

	records := append([]Record(nil), db.Records...)
	slices.SortFunc(records, func(a, b Record) int {
		switch {
		case a.PeriodStart.Before(b.PeriodStart):
			return -1
		case a.PeriodStart.After(b.PeriodStart):
			return 1
		case a.UserID < b.UserID:
			return -1
		case a.UserID > b.UserID:
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
		return Database{}, fmt.Errorf("decode usage database: %w", err)
	}
	if db.SchemaVersion == 0 {
		db.SchemaVersion = usageSchemaVersion
	}
	if db.SchemaVersion != usageSchemaVersion {
		return Database{}, fmt.Errorf("unsupported usage database schema version %d", db.SchemaVersion)
	}
	if db.Records == nil {
		db.Records = []Record{}
	}
	return db, nil
}

func (s *Store) saveLocked(db Database) error {
	if s.path == "" {
		return errors.New("usage database path cannot be empty")
	}
	db.SchemaVersion = usageSchemaVersion
	if db.Records == nil {
		db.Records = []Record{}
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create usage database directory: %w", err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(s.path), "binboi-usage-*.json")
	if err != nil {
		return fmt.Errorf("create usage database temp file: %w", err)
	}
	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("chmod usage database temp file: %w", err)
	}

	enc := json.NewEncoder(tempFile)
	enc.SetIndent("", "  ")
	if err := enc.Encode(db); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("write usage database: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("close usage database temp file: %w", err)
	}
	if err := os.Rename(tempFile.Name(), s.path); err != nil {
		_ = os.Remove(tempFile.Name())
		return fmt.Errorf("replace usage database: %w", err)
	}
	return nil
}
