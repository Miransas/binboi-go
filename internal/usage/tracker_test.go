package usage

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestTrackerFlushAndLimits(t *testing.T) {
	t.Parallel()

	store := NewStore(filepath.Join(t.TempDir(), "usage.json"))
	if err := store.Ensure(); err != nil {
		t.Fatalf("ensure usage store: %v", err)
	}

	tracker := NewTracker(store, Config{
		DatabasePath:         store.Path(),
		FlushIntervalSeconds: 1,
		Period:               PeriodDaily,
		Limits: Limits{
			MaxRequests:      1,
			MaxBytesIn:       1024,
			MaxBytesOut:      2048,
			MaxActiveTunnels: 1,
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	tunnelReservation, err := tracker.ReserveTunnel("user-123")
	if err != nil {
		t.Fatalf("reserve tunnel: %v", err)
	}
	tracker.ConfirmTunnel(tunnelReservation, "tun-1")

	if _, err := tracker.ReserveTunnel("user-123"); err == nil {
		t.Fatal("expected active tunnel limit error")
	}

	requestReservation, err := tracker.ReserveRequest("user-123", 128)
	if err != nil {
		t.Fatalf("reserve request: %v", err)
	}
	tracker.CompleteRequest(requestReservation, 256, 512)

	if _, err := tracker.ReserveRequest("user-123", 64); err == nil {
		t.Fatal("expected request limit error")
	}

	tracker.TrackTunnelClosed("user-123", "tun-1")

	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush usage: %v", err)
	}

	records, err := store.List()
	if err != nil {
		t.Fatalf("list usage records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("usage record count mismatch: got %d want 1", len(records))
	}

	record := records[0]
	if record.UserID != "user-123" {
		t.Fatalf("user ID mismatch: got %q want %q", record.UserID, "user-123")
	}
	if record.RequestCount != 1 {
		t.Fatalf("request count mismatch: got %d want 1", record.RequestCount)
	}
	if record.BytesIn != 256 {
		t.Fatalf("bytes_in mismatch: got %d want 256", record.BytesIn)
	}
	if record.BytesOut != 512 {
		t.Fatalf("bytes_out mismatch: got %d want 512", record.BytesOut)
	}
	if record.ActiveTunnels != 0 {
		t.Fatalf("active tunnels mismatch: got %d want 0", record.ActiveTunnels)
	}
	if record.Period != PeriodDaily {
		t.Fatalf("period mismatch: got %q want %q", record.Period, PeriodDaily)
	}
}

func TestTrackerRunFlushesOnShutdown(t *testing.T) {
	t.Parallel()

	store := NewStore(filepath.Join(t.TempDir(), "usage.json"))
	if err := store.Ensure(); err != nil {
		t.Fatalf("ensure usage store: %v", err)
	}

	tracker := NewTracker(store, Config{
		DatabasePath:         store.Path(),
		FlushIntervalSeconds: 60,
		Period:               PeriodMonthly,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reservation, err := tracker.ReserveRequest("user-456", 0)
	if err != nil {
		t.Fatalf("reserve request: %v", err)
	}
	tracker.CompleteRequest(reservation, 12, 34)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tracker.Run(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tracker did not stop on context cancellation")
	}

	records, err := store.List()
	if err != nil {
		t.Fatalf("list usage records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("usage record count mismatch: got %d want 1", len(records))
	}
}
