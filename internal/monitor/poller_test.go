package monitor

import (
	"testing"
	"time"

	"github.com/parkerg/monitower/internal/db"
)

func newTestPoller(t *testing.T) *Poller {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return NewPoller(conn, NewSSEBroadcaster())
}

// insertServiceRow inserts a service_metrics row directly.
func insertServiceRow(t *testing.T, p *Poller, ts int64, service string, throughput, errorRate, p50, p99 float64) {
	t.Helper()
	_, err := p.db.Exec(
		`INSERT INTO service_metrics (ts, service, throughput, error_rate, p50_latency, p99_latency, active_faults)
		 VALUES (?, ?, ?, ?, ?, ?, '')`,
		ts, service, throughput, errorRate, p50, p99)
	if err != nil {
		t.Fatalf("insert service row: %v", err)
	}
}

// --- Empty DB ---

func TestPoller_EmptyDB_NoSignals(t *testing.T) {
	p := newTestPoller(t)
	if err := p.Tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	snap := p.Snapshot()
	if snap.BaselineReady {
		t.Error("expected baselineReady=false with empty DB")
	}
	if len(snap.Incidents) != 0 {
		t.Errorf("expected 0 incidents, got %d", len(snap.Incidents))
	}
}

// --- Baseline ready after 10 samples ---

func TestPoller_BaselineReady_After10Samples(t *testing.T) {
	p := newTestPoller(t)
	now := time.Now().UnixMilli()

	// Insert 10 rows spread over the last 25 seconds.
	for i := 0; i < 10; i++ {
		ts := now - int64((9-i)*2500)
		insertServiceRow(t, p, ts, "payment", 5.0, 0.01, 50, 100)
	}

	if err := p.Tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	snap := p.Snapshot()
	if !snap.BaselineReady {
		t.Error("expected baselineReady=true after 10 samples")
	}
}

// --- Anomaly detected ---

func TestPoller_AnomalyDetected_AfterSpike(t *testing.T) {
	p := newTestPoller(t)
	now := time.Now().UnixMilli()

	// Seed 10 healthy rows in the last 25s.
	for i := 0; i < 10; i++ {
		ts := now - int64((10-i)*2500)
		insertServiceRow(t, p, ts, "payment", 5.0, 0.01, 50, 100)
	}

	// Tick once to establish baseline.
	_ = p.Tick()

	// Insert an anomalous row: high error rate.
	insertServiceRow(t, p, now-100, "payment", 5.0, 0.80, 50, 600)

	// Tick again — anomaly should be detected.
	_ = p.Tick()

	active := p.correlator.ActiveSignals()
	found := false
	for _, sig := range active {
		if sig.Service == "payment" && sig.Metric == "error_rate" {
			found = true
		}
	}
	if !found {
		t.Error("expected error_rate anomaly signal for payment after spike")
	}
}

// --- Snapshot contains service statuses ---

func TestPoller_Snapshot_ContainsServices(t *testing.T) {
	p := newTestPoller(t)
	now := time.Now().UnixMilli()
	insertServiceRow(t, p, now-1000, "order", 3.0, 0.0, 30, 80)

	_ = p.Tick()
	snap := p.Snapshot()

	found := false
	for _, svc := range snap.Services {
		if svc.Name == "order" {
			found = true
			if svc.Status != "healthy" {
				t.Errorf("expected order status healthy, got %s", svc.Status)
			}
		}
	}
	if !found {
		t.Error("expected order in snapshot services")
	}
}

// --- Resolution via ResolutionTracker integration ---

func TestPoller_ResolutionTracker_ResolvesThroughStore(t *testing.T) {
	// Test the resolution path directly: create an incident, then simulate 2 clean cycles.
	p := newTestPoller(t)

	// Create an incident directly.
	ev := makeEvent("payment", "poison-pill")
	_, _ = p.incStore.Reconcile([]CorrelatedEvent{ev})
	actives, _ := p.incStore.ListActive()
	if len(actives) != 1 {
		t.Fatalf("expected 1 active incident, got %d", len(actives))
	}

	now := time.Now().UnixMilli()

	// Cycle 1: no active signals → count = 1.
	toResolve := p.resolver.Check(actives, nil)
	if len(toResolve) != 0 {
		t.Fatal("should not resolve after 1 clean cycle")
	}

	// Cycle 2: no active signals → count = 2 → resolve.
	toResolve = p.resolver.Check(actives, nil)
	if len(toResolve) != 1 {
		t.Fatalf("expected 1 incident to resolve, got %d", len(toResolve))
	}
	_ = p.incStore.MarkResolved(toResolve[0], now)

	actives, _ = p.incStore.ListActive()
	if len(actives) != 0 {
		t.Errorf("expected 0 active incidents after resolution, got %d", len(actives))
	}
}
