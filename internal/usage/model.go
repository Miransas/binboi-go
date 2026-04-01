package usage

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const usageSchemaVersion = 1

// Period controls the reporting and limit window for usage tracking.
type Period string

const (
	PeriodDaily   Period = "daily"
	PeriodMonthly Period = "monthly"
)

// Limits describes simple plan caps enforced by the daemon.
type Limits struct {
	MaxRequests      int64 `json:"max_requests,omitempty"`
	MaxBytesIn       int64 `json:"max_bytes_in,omitempty"`
	MaxBytesOut      int64 `json:"max_bytes_out,omitempty"`
	MaxActiveTunnels int   `json:"max_active_tunnels,omitempty"`
}

// Config configures in-memory aggregation and durable flush behavior.
type Config struct {
	DatabasePath         string `json:"database_path"`
	FlushIntervalSeconds int    `json:"flush_interval_seconds"`
	Period               Period `json:"period"`
	Limits               Limits `json:"limits"`
}

// Record is the durable per-user usage snapshot.
type Record struct {
	UserID        string    `json:"user_id"`
	Period        Period    `json:"period"`
	PeriodStart   time.Time `json:"period_start"`
	RequestCount  int64     `json:"request_count"`
	BytesIn       int64     `json:"bytes_in"`
	BytesOut      int64     `json:"bytes_out"`
	ActiveTunnels int       `json:"active_tunnels"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Database is the versioned on-disk usage schema.
type Database struct {
	SchemaVersion int      `json:"schema_version"`
	Records       []Record `json:"records"`
}

// LimitError reports that a user exceeded a configured plan limit.
type LimitError struct {
	UserID   string
	Resource string
	Limit    int64
	Current  int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("usage limit exceeded for %s: current=%d limit=%d", e.Resource, e.Current, e.Limit)
}

// Normalize applies production-minded defaults to usage tracking.
func (c Config) Normalize() Config {
	if c.FlushIntervalSeconds <= 0 {
		c.FlushIntervalSeconds = 10
	}
	if c.Period == "" {
		c.Period = PeriodMonthly
	}
	return c
}

// Validate ensures the usage configuration is usable.
func (c Config) Validate() error {
	normalized := c.Normalize()
	if strings.TrimSpace(normalized.DatabasePath) == "" {
		return errors.New("database_path is required")
	}
	switch normalized.Period {
	case PeriodDaily, PeriodMonthly:
	default:
		return fmt.Errorf("unsupported usage period %q", normalized.Period)
	}
	if normalized.FlushIntervalSeconds <= 0 {
		return errors.New("flush_interval_seconds must be greater than zero")
	}
	if err := normalized.Limits.Validate(); err != nil {
		return err
	}
	return nil
}

// Validate ensures the configured limits are non-negative.
func (l Limits) Validate() error {
	if l.MaxRequests < 0 {
		return errors.New("max_requests cannot be negative")
	}
	if l.MaxBytesIn < 0 {
		return errors.New("max_bytes_in cannot be negative")
	}
	if l.MaxBytesOut < 0 {
		return errors.New("max_bytes_out cannot be negative")
	}
	if l.MaxActiveTunnels < 0 {
		return errors.New("max_active_tunnels cannot be negative")
	}
	return nil
}
