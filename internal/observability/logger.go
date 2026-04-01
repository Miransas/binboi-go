package observability

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds a slog logger from simple level and format strings.
func NewLogger(level, format string) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	switch strings.ToLower(format) {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q", format)
	}
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
