package monitor

import (
	"database/sql"
	"testing"

	"github.com/parkerg/monitower/internal/db"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func makeEvent(rootService, faultType string) CorrelatedEvent {
	ft := faultType
	return CorrelatedEvent{
		StartTS:     1000,
		RootService: rootService,
		FaultType:   ft,
		Severity:    "warning",
		RootCause:   rootService + ": anomaly detected",
		Affected:    []string{rootService},
		Timeline:    []TimelineEvent{{TS: 1000, Message: "detected"}},
	}
}

// --- Create ---

func TestIncidents_CreateNewIncident(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))
	_, err := store.Reconcile([]CorrelatedEvent{makeEvent("payment", "poison-pill")})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	actives, err := store.ListActive()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(actives) != 1 {
		t.Fatalf("expected 1 active incident, got %d", len(actives))
	}
	inc := actives[0]
	if inc.RootService != "payment" {
		t.Errorf("expected root_service payment, got %s", inc.RootService)
	}
	if inc.FaultType == nil || *inc.FaultType != "poison-pill" {
		t.Errorf("expected fault_type poison-pill, got %v", inc.FaultType)
	}
	if inc.Status != "active" {
		t.Errorf("expected status active, got %s", inc.Status)
	}
}

// --- Deduplication ---

func TestIncidents_DedupSameRootAndFaultType(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))

	// First reconcile creates the incident.
	_, err := store.Reconcile([]CorrelatedEvent{makeEvent("payment", "poison-pill")})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	// Second reconcile with same root+faultType should not create a second incident.
	_, err = store.Reconcile([]CorrelatedEvent{makeEvent("payment", "poison-pill")})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	actives, _ := store.ListActive()
	if len(actives) != 1 {
		t.Fatalf("expected 1 incident after dedup, got %d", len(actives))
	}
}

func TestIncidents_DedupAppendsTimeline(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))

	ev1 := makeEvent("payment", "")
	ev1.Timeline = []TimelineEvent{{TS: 1000, Message: "first"}}
	_, _ = store.Reconcile([]CorrelatedEvent{ev1})

	ev2 := makeEvent("payment", "")
	ev2.Timeline = []TimelineEvent{{TS: 3000, Message: "second"}}
	_, _ = store.Reconcile([]CorrelatedEvent{ev2})

	actives, _ := store.ListActive()
	if len(actives) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(actives))
	}
	if len(actives[0].Timeline) != 2 {
		t.Errorf("expected 2 timeline events, got %d", len(actives[0].Timeline))
	}
}

// --- Resolution ---

func TestIncidents_ResolveAfterTwoCleanCycles(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))
	tracker := NewResolutionTracker()

	_, _ = store.Reconcile([]CorrelatedEvent{makeEvent("payment", "")})
	actives, _ := store.ListActive()

	// Cycle 1: no active signals — count becomes 1 (not yet resolved).
	toResolve := tracker.Check(actives, nil)
	if len(toResolve) != 0 {
		t.Fatalf("expected no resolution after 1 clean cycle, got %d", len(toResolve))
	}

	// Cycle 2: no active signals — count becomes 2 → resolve.
	toResolve = tracker.Check(actives, nil)
	if len(toResolve) != 1 {
		t.Fatalf("expected 1 resolution after 2 clean cycles, got %d", len(toResolve))
	}

	if err := store.MarkResolved(toResolve[0], 9000); err != nil {
		t.Fatalf("mark resolved: %v", err)
	}

	actives, _ = store.ListActive()
	if len(actives) != 0 {
		t.Fatal("expected no active incidents after resolution")
	}
}

func TestIncidents_PartialResolution_StaysOpen(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))
	tracker := NewResolutionTracker()

	ev := makeEvent("payment", "")
	ev.Affected = []string{"payment", "fulfillment"}
	_, _ = store.Reconcile([]CorrelatedEvent{ev})
	actives, _ := store.ListActive()

	// fulfillment still anomalous.
	signals := []AnomalySignal{makeServiceSig(5000, "fulfillment", "throughput")}
	toResolve := tracker.Check(actives, signals)
	if len(toResolve) != 0 {
		t.Fatalf("expected no resolution when downstream still anomalous, got %d", len(toResolve))
	}

	actives, _ = store.ListActive()
	if len(actives) != 1 {
		t.Fatal("incident should still be active")
	}
}

// --- Resolved not re-opened ---

func TestIncidents_ResolvedIncidentNotReopened(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))
	tracker := NewResolutionTracker()

	_, _ = store.Reconcile([]CorrelatedEvent{makeEvent("payment", "")})
	actives, _ := store.ListActive()

	// Resolve the incident.
	tracker.Check(actives, nil)
	toResolve := tracker.Check(actives, nil)
	_ = store.MarkResolved(toResolve[0], 9000)

	// Now reconcile with the same event again — should create a NEW incident, not reuse the resolved one.
	_, _ = store.Reconcile([]CorrelatedEvent{makeEvent("payment", "")})
	actives, _ = store.ListActive()
	if len(actives) != 1 {
		t.Fatalf("expected 1 new active incident, got %d", len(actives))
	}
}

// --- GetByID ---

func TestIncidents_GetByID(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))
	_, _ = store.Reconcile([]CorrelatedEvent{makeEvent("order", "")})
	actives, _ := store.ListActive()
	if len(actives) == 0 {
		t.Fatal("no incident created")
	}

	inc, err := store.GetByID(actives[0].ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if inc == nil {
		t.Fatal("expected incident, got nil")
	}
	if inc.RootService != "order" {
		t.Errorf("expected order, got %s", inc.RootService)
	}
}

// TestIncidents_DLQSignalKeepsIncidentOpen verifies that a DLQ signal for
// payment-dlq prevents the payment incident from resolving prematurely.
// This guards against the alternating healthy/degraded bug where the DLQ
// anomaly maps to the queue name ("payment-dlq") but the incident's Affected
// list contains the owning service ("payment").
func TestIncidents_DLQSignalKeepsIncidentOpen(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))
	tracker := NewResolutionTracker()

	ev := makeEvent("payment", "poison-pill")
	ev.Affected = []string{"payment"}
	_, _ = store.Reconcile([]CorrelatedEvent{ev})
	actives, _ := store.ListActive()

	// DLQ signal still active after fault stops (depth > 0).
	dlqSig := AnomalySignal{
		TS:     5000,
		Queue:  "payment-dlq",
		Metric: "dlq_depth",
		Value:  3,
		Direction: "high",
	}

	// Both cycles should NOT resolve — the DLQ owner (payment) is still anomalous.
	toResolve := tracker.Check(actives, []AnomalySignal{dlqSig})
	if len(toResolve) != 0 {
		t.Fatalf("cycle 1: should not resolve while DLQ has depth, got %d to resolve", len(toResolve))
	}
	toResolve = tracker.Check(actives, []AnomalySignal{dlqSig})
	if len(toResolve) != 0 {
		t.Fatalf("cycle 2: should not resolve while DLQ has depth, got %d to resolve", len(toResolve))
	}

	// Once DLQ drains, incident should resolve after 2 clean cycles.
	toResolve = tracker.Check(actives, nil)
	if len(toResolve) != 0 {
		t.Fatalf("clean cycle 1: should not resolve yet, got %d", len(toResolve))
	}
	toResolve = tracker.Check(actives, nil)
	if len(toResolve) != 1 {
		t.Fatalf("clean cycle 2: expected 1 resolution, got %d", len(toResolve))
	}
}

func TestIncidents_GetByID_NotFound(t *testing.T) {
	store := NewIncidentStore(openTestDB(t))
	inc, err := store.GetByID(9999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inc != nil {
		t.Fatal("expected nil for unknown id")
	}
}
