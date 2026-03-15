package monitor

import (
	"testing"
)

// makeServiceSig creates a service AnomalySignal for testing.
func makeServiceSig(ts int64, service, metric string) AnomalySignal {
	return AnomalySignal{TS: ts, Service: service, Metric: metric, Direction: "high", Value: 1, Deviation: 5}
}

func newCorrelator(signals []AnomalySignal) *Correlator {
	c := NewCorrelator()
	if len(signals) > 0 {
		c.AddSignals(signals[len(signals)-1].TS+1000, signals)
	}
	return c
}

// --- Single signal promotion ---

func TestCorrelator_SingleNonDLQSignalNotPromoted(t *testing.T) {
	c := newCorrelator([]AnomalySignal{makeServiceSig(1000, "payment", "error_rate")})
	events := c.Correlate(nil)
	if len(events) != 0 {
		t.Fatalf("expected 0 events for single non-DLQ signal, got %d", len(events))
	}
}

func TestCorrelator_SingleDLQSignalPromoted(t *testing.T) {
	sig := AnomalySignal{TS: 1000, Queue: "payment-dlq", Metric: "dlq_depth", Value: 3, Deviation: 3}
	c := newCorrelator([]AnomalySignal{sig})
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for DLQ signal, got %d", len(events))
	}
}

// --- Grouping window ---

func TestCorrelator_SignalsWithin10sGrouped(t *testing.T) {
	sigs := []AnomalySignal{
		makeServiceSig(1000, "payment", "error_rate"),
		makeServiceSig(8000, "fulfillment", "throughput"),
	}
	c := newCorrelator(sigs)
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (same group), got %d", len(events))
	}
	if len(events[0].Signals) != 2 {
		t.Errorf("expected 2 signals in group, got %d", len(events[0].Signals))
	}
}

func TestCorrelator_SignalsOver10sApartSeparateGroups(t *testing.T) {
	sigs := []AnomalySignal{
		makeServiceSig(1000, "payment", "error_rate"),
		makeServiceSig(5000, "order", "error_rate"), // within 10s of first
		// 12s after first signal → new group; must also form a group (needs DLQ or 2+ signals)
		{TS: 13000, Queue: "payment-dlq", Metric: "dlq_depth", Value: 5, Deviation: 5},
	}
	c := newCorrelator(sigs)
	events := c.Correlate(nil)
	if len(events) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(events))
	}
}

// --- Root cause: topology depth ---

func TestCorrelator_RootCause_TopologyDepth(t *testing.T) {
	sigs := []AnomalySignal{
		makeServiceSig(1000, "fulfillment", "throughput"),
		makeServiceSig(2000, "payment", "error_rate"),
	}
	c := newCorrelator(sigs)
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].RootService != "payment" {
		t.Errorf("expected root payment (depth 2), got %s", events[0].RootService)
	}
}

func TestCorrelator_RootCause_TieBreakByTimestamp(t *testing.T) {
	// Both fulfillment and notification are depth 3.
	sigs := []AnomalySignal{
		makeServiceSig(3000, "notification", "error_rate"),
		makeServiceSig(1000, "fulfillment", "error_rate"),
	}
	c := newCorrelator(sigs)
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// fulfillment has earlier TS → wins.
	if events[0].RootService != "fulfillment" {
		t.Errorf("expected root fulfillment (earlier ts), got %s", events[0].RootService)
	}
}

// --- Root cause: fault_events override ---

func TestCorrelator_RootCause_FaultEventsOverride(t *testing.T) {
	// Signals suggest fulfillment, but fault_event says payment started 3s before group.
	sigs := []AnomalySignal{
		makeServiceSig(5000, "fulfillment", "throughput"),
		makeServiceSig(6000, "notification", "error_rate"),
	}
	c := newCorrelator(sigs)
	faults := []FaultEvent{
		{TS: 3000, FaultType: "poison-pill", Action: "start", Origin: "payment"},
	}
	events := c.Correlate(faults)
	// The fault_events override doesn't fire here because "payment" is not in the signal set.
	// Let's test with payment in the signal set.
	sigs2 := []AnomalySignal{
		makeServiceSig(5000, "fulfillment", "throughput"),
		makeServiceSig(5500, "payment", "error_rate"),
	}
	c2 := newCorrelator(sigs2)
	events2 := c2.Correlate(faults)
	if len(events2) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events2))
	}
	if events2[0].RootService != "payment" {
		t.Errorf("expected fault_events override to payment, got %s", events2[0].RootService)
	}
	if events2[0].FaultType != "poison-pill" {
		t.Errorf("expected fault_type poison-pill, got %s", events2[0].FaultType)
	}
	_ = events
}

// --- Root cause: queue → owning service ---

func TestCorrelator_RootCause_QueueSignalMapsToOwner(t *testing.T) {
	// order-queue signal + payment signal → payment owns order-queue but topology: payment depth=2 > order depth=1
	// Use payment-queue (owned by payment, depth 2) + fulfillment (depth 3) → root should be payment.
	sigs := []AnomalySignal{
		{TS: 1000, Queue: "payment-queue", Metric: "queue_depth", Value: 100, Deviation: 5},
		makeServiceSig(2000, "fulfillment", "throughput"),
	}
	c := newCorrelator(sigs)
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// payment-queue owned by payment (depth 2), fulfillment is depth 3 → root = payment
	if events[0].RootService != "payment" {
		t.Errorf("expected root payment (queue owner), got %s", events[0].RootService)
	}
}

// --- DLQ-only signal ---

func TestCorrelator_DLQOnly_RootAttributedToOwner(t *testing.T) {
	sig := AnomalySignal{TS: 1000, Queue: "notification-dlq", Metric: "dlq_depth", Value: 2, Deviation: 2}
	c := newCorrelator([]AnomalySignal{sig})
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].RootService != "notification" {
		t.Errorf("expected root notification (dlq owner), got %s", events[0].RootService)
	}
	if !containsStr(events[0].RootCause, "silent failure") {
		t.Errorf("expected silent failure note in root cause, got: %s", events[0].RootCause)
	}
}

// --- Blast radius ---

func TestCorrelator_BlastRadius_IncludesDownstreamWithSignals(t *testing.T) {
	sigs := []AnomalySignal{
		makeServiceSig(1000, "payment", "error_rate"),
		makeServiceSig(2000, "fulfillment", "throughput"),
	}
	c := newCorrelator(sigs)
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	affected := events[0].Affected
	if !containsStr(sliceToStr(affected), "payment") {
		t.Error("expected payment in affected")
	}
	if !containsStr(sliceToStr(affected), "fulfillment") {
		t.Error("expected fulfillment in affected")
	}
}

// --- Severity ---

func TestCorrelator_Severity_CriticalOnHighDLQ(t *testing.T) {
	sig := AnomalySignal{TS: 1000, Queue: "payment-dlq", Metric: "dlq_depth", Value: 60, Deviation: 60}
	c := newCorrelator([]AnomalySignal{sig})
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Severity != "critical" {
		t.Errorf("expected critical severity, got %s", events[0].Severity)
	}
}

func TestCorrelator_Severity_WarningDefault(t *testing.T) {
	sigs := []AnomalySignal{
		makeServiceSig(1000, "order", "error_rate"),
		makeServiceSig(2000, "payment", "error_rate"),
	}
	c := newCorrelator(sigs)
	events := c.Correlate(nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Severity != "warning" {
		t.Errorf("expected warning severity, got %s", events[0].Severity)
	}
}

// --- Signal pruning ---

func TestCorrelator_StaleSignalsPruned(t *testing.T) {
	c := NewCorrelator()
	// Add old signal at t=0ms.
	c.AddSignals(1000, []AnomalySignal{makeServiceSig(0, "payment", "error_rate")})
	// Advance time by 31 seconds — old signal should be pruned.
	c.AddSignals(31001, []AnomalySignal{makeServiceSig(31000, "order", "error_rate")})
	active := c.ActiveSignals()
	for _, sig := range active {
		if sig.TS == 0 {
			t.Fatal("expected stale signal to be pruned")
		}
	}
}

// --- helpers ---

func containsStr(s, sub string) bool {
	return len(s) > 0 && len(sub) > 0 && (s == sub || len(s) >= len(sub) && findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func sliceToStr(ss []string) string {
	result := ""
	for _, s := range ss {
		result += s + " "
	}
	return result
}
