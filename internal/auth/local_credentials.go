package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalCredentials stores the CLI's locally saved authentication token.
type LocalCredentials struct {
	Token     string    `json:"token,omitempty"`
	SavedAt   time.Time `json:"saved_at,omitempty"`
	TokenHint string    `json:"token_hint,omitempty"`
}

// DefaultLocalCredentialsPath returns the standard CLI credentials file path.
func DefaultLocalCredentialsPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(configDir, "binboi", "credentials.json"), nil
}

// LoadLocalCredentials reads the CLI credentials file.
func LoadLocalCredentials(path string) (LocalCredentials, error) {
	if strings.TrimSpace(path) == "" {
		return LocalCredentials{}, errors.New("credentials path cannot be empty")
	}

	file, err := os.Open(path)
	if err != nil {
		return LocalCredentials{}, err
	}
	defer file.Close()

	var creds LocalCredentials
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&creds); err != nil {
		return LocalCredentials{}, fmt.Errorf("decode credentials: %w", err)
	}
	return creds, nil
}

// SaveLocalCredentials writes the CLI credentials file with secure permissions.
func SaveLocalCredentials(path string, creds LocalCredentials) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("credentials path cannot be empty")
	}
	if strings.TrimSpace(creds.Token) == "" {
		return errors.New("token cannot be empty")
	}

	if creds.SavedAt.IsZero() {
		creds.SavedAt = time.Now().UTC()
	}
	if creds.TokenHint == "" {
		if prefix, err := PrefixFromRawToken(creds.Token); err == nil {
			creds.TokenHint = prefix
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(path), "binboi-credentials-*.json")
	if err != nil {
		return fmt.Errorf("create credentials temp file: %w", err)
	}
	defer os.Remove(tempFile.Name())

	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod credentials temp file: %w", err)
	}

	enc := json.NewEncoder(tempFile)
	enc.SetIndent("", "  ")
	if err := enc.Encode(creds); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("encode credentials: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close credentials temp file: %w", err)
	}
	if err := os.Rename(tempFile.Name(), path); err != nil {
		return fmt.Errorf("replace credentials file: %w", err)
	}
	return nil
}

// ClearLocalCredentials removes the CLI credentials file if it exists.
func ClearLocalCredentials(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("credentials path cannot be empty")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove credentials file: %w", err)
	}
	return nil
}
