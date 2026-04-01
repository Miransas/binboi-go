package usage

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type usageBucket struct {
	record           Record
	reservedRequests int64
	reservedBytesIn  int64
	dirty            bool
}

// TunnelReservation reserves capacity for a new active tunnel before registration completes.
type TunnelReservation struct {
	userID   string
	acquired bool
}

// RequestReservation reserves request quota before the forward path starts.
type RequestReservation struct {
	userID           string
	bucketKey        string
	estimatedBytesIn int64
	acquired         bool
}

// Tracker aggregates usage in memory and periodically flushes it to durable storage.
type Tracker struct {
	store         *Store
	logger        *slog.Logger
	period        Period
	flushInterval time.Duration
	limits        Limits

	mu              sync.Mutex
	buckets         map[string]*usageBucket
	activeTunnels   map[string]map[string]struct{}
	reservedTunnels map[string]int
}

// NewTracker constructs a usage tracker with in-memory aggregation.
func NewTracker(store *Store, cfg Config, logger *slog.Logger) *Tracker {
	normalized := cfg.Normalize()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(ioDiscard{}, nil))
	}

	return &Tracker{
		store:           store,
		logger:          logger,
		period:          normalized.Period,
		flushInterval:   time.Duration(normalized.FlushIntervalSeconds) * time.Second,
		limits:          normalized.Limits,
		buckets:         make(map[string]*usageBucket),
		activeTunnels:   make(map[string]map[string]struct{}),
		reservedTunnels: make(map[string]int),
	}
}

// Run periodically flushes dirty usage snapshots until the context is canceled.
func (t *Tracker) Run(ctx context.Context) {
	ticker := time.NewTicker(t.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if err := t.Flush(); err != nil {
				t.logger.Warn("final usage flush failed", "error", err)
			}
			return
		case <-ticker.C:
			if err := t.Flush(); err != nil {
				t.logger.Warn("usage flush failed", "error", err)
			}
		}
	}
}

// ReserveTunnel checks the active tunnel limit and reserves capacity.
func (t *Tracker) ReserveTunnel(userID string) (TunnelReservation, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return TunnelReservation{}, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	currentActive := len(t.activeTunnels[userID]) + t.reservedTunnels[userID]
	if t.limits.MaxActiveTunnels > 0 && currentActive+1 > t.limits.MaxActiveTunnels {
		err := &LimitError{
			UserID:   userID,
			Resource: "active_tunnels",
			Limit:    int64(t.limits.MaxActiveTunnels),
			Current:  int64(currentActive + 1),
		}
		t.logger.Warn("usage limit hit",
			"user_id", userID,
			"resource", err.Resource,
			"limit", err.Limit,
			"current", err.Current,
		)
		return TunnelReservation{}, err
	}

	t.reservedTunnels[userID]++
	return TunnelReservation{userID: userID, acquired: true}, nil
}

// ReleaseTunnelReservation frees a previously reserved tunnel slot.
func (t *Tracker) ReleaseTunnelReservation(reservation TunnelReservation) {
	if !reservation.acquired || reservation.userID == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reservedTunnels[reservation.userID] > 0 {
		t.reservedTunnels[reservation.userID]--
	}
}

// ConfirmTunnel marks a tunnel as active after successful registration.
func (t *Tracker) ConfirmTunnel(reservation TunnelReservation, tunnelID string) {
	if !reservation.acquired || reservation.userID == "" || tunnelID == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.reservedTunnels[reservation.userID] > 0 {
		t.reservedTunnels[reservation.userID]--
	}
	active := t.ensureActiveSetLocked(reservation.userID)
	active[tunnelID] = struct{}{}
	t.touchBucketLocked(reservation.userID)
}

// RestoreTunnel marks a resumed tunnel active without consuming new tunnel quota.
func (t *Tracker) RestoreTunnel(userID, tunnelID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" || tunnelID == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	active := t.ensureActiveSetLocked(userID)
	active[tunnelID] = struct{}{}
	t.touchBucketLocked(userID)
}

// TrackTunnelClosed removes a tunnel from the active set.
func (t *Tracker) TrackTunnelClosed(userID, tunnelID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" || tunnelID == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if active, ok := t.activeTunnels[userID]; ok {
		delete(active, tunnelID)
		if len(active) == 0 {
			delete(t.activeTunnels, userID)
		}
	}
	t.touchBucketLocked(userID)
}

// ReserveRequest checks plan limits and reserves per-request quota.
func (t *Tracker) ReserveRequest(userID string, estimatedBytesIn int64) (RequestReservation, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return RequestReservation{}, nil
	}
	if estimatedBytesIn < 0 {
		estimatedBytesIn = 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	key, bucket := t.currentBucketLocked(userID, time.Now().UTC())
	projectedRequests := bucket.record.RequestCount + bucket.reservedRequests + 1
	if t.limits.MaxRequests > 0 && projectedRequests > t.limits.MaxRequests {
		err := &LimitError{
			UserID:   userID,
			Resource: "requests",
			Limit:    t.limits.MaxRequests,
			Current:  projectedRequests,
		}
		t.logger.Warn("usage limit hit",
			"user_id", userID,
			"resource", err.Resource,
			"limit", err.Limit,
			"current", err.Current,
		)
		return RequestReservation{}, err
	}

	if t.limits.MaxBytesIn > 0 {
		projectedBytesIn := bucket.record.BytesIn + bucket.reservedBytesIn + estimatedBytesIn
		if projectedBytesIn > t.limits.MaxBytesIn {
			err := &LimitError{
				UserID:   userID,
				Resource: "bytes_in",
				Limit:    t.limits.MaxBytesIn,
				Current:  projectedBytesIn,
			}
			t.logger.Warn("usage limit hit",
				"user_id", userID,
				"resource", err.Resource,
				"limit", err.Limit,
				"current", err.Current,
			)
			return RequestReservation{}, err
		}
	}

	if t.limits.MaxBytesOut > 0 && bucket.record.BytesOut >= t.limits.MaxBytesOut {
		err := &LimitError{
			UserID:   userID,
			Resource: "bytes_out",
			Limit:    t.limits.MaxBytesOut,
			Current:  bucket.record.BytesOut,
		}
		t.logger.Warn("usage limit hit",
			"user_id", userID,
			"resource", err.Resource,
			"limit", err.Limit,
			"current", err.Current,
		)
		return RequestReservation{}, err
	}

	bucket.reservedRequests++
	bucket.reservedBytesIn += estimatedBytesIn
	return RequestReservation{
		userID:           userID,
		bucketKey:        key,
		estimatedBytesIn: estimatedBytesIn,
		acquired:         true,
	}, nil
}

// ReleaseRequest frees a request reservation without incrementing counters.
func (t *Tracker) ReleaseRequest(reservation RequestReservation) {
	if !reservation.acquired || reservation.userID == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	bucket, ok := t.buckets[reservation.bucketKey]
	if !ok {
		return
	}
	if bucket.reservedRequests > 0 {
		bucket.reservedRequests--
	}
	if bucket.reservedBytesIn >= reservation.estimatedBytesIn {
		bucket.reservedBytesIn -= reservation.estimatedBytesIn
	} else {
		bucket.reservedBytesIn = 0
	}
}

// CompleteRequest finalizes a request and records actual bytes transferred.
func (t *Tracker) CompleteRequest(reservation RequestReservation, bytesIn, bytesOut int64) {
	if !reservation.acquired || reservation.userID == "" {
		return
	}
	if bytesIn < 0 {
		bytesIn = 0
	}
	if bytesOut < 0 {
		bytesOut = 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	bucket, ok := t.buckets[reservation.bucketKey]
	if !ok {
		_, bucket = t.currentBucketLocked(reservation.userID, time.Now().UTC())
	}
	if bucket.reservedRequests > 0 {
		bucket.reservedRequests--
	}
	if bucket.reservedBytesIn >= reservation.estimatedBytesIn {
		bucket.reservedBytesIn -= reservation.estimatedBytesIn
	} else {
		bucket.reservedBytesIn = 0
	}

	bucket.record.RequestCount++
	bucket.record.BytesIn += bytesIn
	bucket.record.BytesOut += bytesOut
	bucket.record.ActiveTunnels = len(t.activeTunnels[reservation.userID])
	bucket.record.UpdatedAt = time.Now().UTC()
	bucket.dirty = true
}

// Flush persists all dirty usage snapshots in one durable write.
func (t *Tracker) Flush() error {
	if t.store == nil {
		return nil
	}

	type flushItem struct {
		key    string
		record Record
	}

	now := time.Now().UTC()
	t.mu.Lock()
	flushItems := make([]flushItem, 0, len(t.buckets))
	for key, bucket := range t.buckets {
		if !bucket.dirty {
			continue
		}
		bucket.record.ActiveTunnels = len(t.activeTunnels[bucket.record.UserID])
		bucket.record.UpdatedAt = now
		flushItems = append(flushItems, flushItem{key: key, record: bucket.record})
		bucket.dirty = false
	}
	t.mu.Unlock()

	if len(flushItems) == 0 {
		return nil
	}

	records := make([]Record, 0, len(flushItems))
	for _, item := range flushItems {
		records = append(records, item.record)
	}

	if err := t.store.UpsertMany(records); err != nil {
		t.mu.Lock()
		for _, item := range flushItems {
			if bucket, ok := t.buckets[item.key]; ok {
				bucket.dirty = true
			}
		}
		t.mu.Unlock()
		return err
	}

	t.logger.Info("flushed usage snapshots",
		"records", len(records),
	)
	return nil
}

func (t *Tracker) currentBucketLocked(userID string, now time.Time) (string, *usageBucket) {
	periodStart := t.periodStart(now)
	key := bucketKey(userID, t.period, periodStart)
	if bucket, ok := t.buckets[key]; ok {
		bucket.record.ActiveTunnels = len(t.activeTunnels[userID])
		return key, bucket
	}

	bucket := &usageBucket{
		record: Record{
			UserID:        userID,
			Period:        t.period,
			PeriodStart:   periodStart,
			ActiveTunnels: len(t.activeTunnels[userID]),
			UpdatedAt:     now,
		},
	}
	t.buckets[key] = bucket
	return key, bucket
}

func (t *Tracker) touchBucketLocked(userID string) {
	if userID == "" {
		return
	}
	_, bucket := t.currentBucketLocked(userID, time.Now().UTC())
	bucket.record.ActiveTunnels = len(t.activeTunnels[userID])
	bucket.record.UpdatedAt = time.Now().UTC()
	bucket.dirty = true
}

func (t *Tracker) ensureActiveSetLocked(userID string) map[string]struct{} {
	active, ok := t.activeTunnels[userID]
	if ok {
		return active
	}
	active = make(map[string]struct{})
	t.activeTunnels[userID] = active
	return active
}

func (t *Tracker) periodStart(now time.Time) time.Time {
	now = now.UTC()
	switch t.period {
	case PeriodDaily:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	default:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
}

func bucketKey(userID string, period Period, start time.Time) string {
	return fmt.Sprintf("%s|%s|%s", userID, period, start.UTC().Format(time.RFC3339))
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
