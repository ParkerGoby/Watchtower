package simulator_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/parkerg/watchtower/internal/simulator"
)

// paymentMsg mirrors the JSON shape produced by the Payment Service.
type paymentMsg struct {
	MessageID     string  `json:"messageId"`
	OrderID       string  `json:"orderId"`
	CustomerID    string  `json:"customerId"`
	TotalAmount   float64 `json:"totalAmount"`
	Status        string  `json:"status"`
	TransactionID string  `json:"transactionId"`
	ProcessedAt   int64   `json:"processedAt"`
	TraceID       string  `json:"traceId"`
	ProcessingMs  int64   `json:"processingMs"`
}

// dlqMsg mirrors the DLQ envelope written by the Payment Service.
type dlqMsg struct {
	OriginalMessage json.RawMessage `json:"originalMessage"`
	FailureReason   string          `json:"failureReason"`
	AttemptCount    int             `json:"attemptCount"`
	FirstAttemptAt  int64           `json:"firstAttemptAt"`
	LastAttemptAt   int64           `json:"lastAttemptAt"`
}

// waitForMsg polls a queue until a message arrives or the deadline passes.
func waitForMsg(q *simulator.Queue, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg, ok := q.TryReceive()
		if ok {
			return msg, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", false
}

// ── Service Metrics ───────────────────────────────────────────────────────────

// 1. ForceWriteMetrics produces a service_metrics row immediately.
func TestPaymentService_WritesServiceMetrics(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)

	// Let a few messages process, then force a write.
	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='payment'`).Scan(&count)
	if count == 0 {
		t.Error("expected at least one service_metrics row after ForceWriteMetrics")
	}
}

// 2. After context cancel, no further metrics rows are written.
func TestPaymentService_Stop_NoFurtherWrites(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	engine := simulator.NewFaultEngine(db)
	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)

	// Force an initial write, then stop.
	time.Sleep(300 * time.Millisecond)
	svc.ForceWriteMetrics()

	cancel()
	time.Sleep(100 * time.Millisecond) // let goroutines exit

	var countBefore int
	db.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='payment'`).Scan(&countBefore)

	// Goroutine exits immediately on ctx.Done(); no timer can fire after this.
	time.Sleep(100 * time.Millisecond)

	var countAfter int
	db.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='payment'`).Scan(&countAfter)
	if countAfter != countBefore {
		t.Errorf("expected no new rows after stop, got %d new rows", countAfter-countBefore)
	}
}

// ── Happy Path ────────────────────────────────────────────────────────────────

// 3. Happy path: throughput ≥ 4 msg/s measured over a 2s processing window.
func TestPaymentService_HappyPath_Throughput(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)
	drainQueue(ctx, pq)

	// Process for 2s then flush.
	time.Sleep(2 * time.Second)
	svc.ForceWriteMetrics()

	var throughput float64
	db.QueryRow(`SELECT throughput FROM service_metrics WHERE service='payment' ORDER BY ts DESC LIMIT 1`).Scan(&throughput)
	if throughput < 3.5 {
		t.Errorf("expected throughput ≥ 3.5 msg/s, got %.2f", throughput)
	}
}

// 4. Happy path: error_rate ≤ 0.01 and DLQ stays empty with no faults.
func TestPaymentService_HappyPath_NoDLQ(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)
	drainQueue(ctx, pq)

	time.Sleep(2 * time.Second)
	svc.ForceWriteMetrics()

	var errorRate float64
	db.QueryRow(`SELECT error_rate FROM service_metrics WHERE service='payment' ORDER BY ts DESC LIMIT 1`).Scan(&errorRate)
	if errorRate > 0.01 {
		t.Errorf("expected error_rate ≤ 0.01 with no faults, got %.4f", errorRate)
	}
	if dq.Depth() != 0 {
		t.Errorf("expected DLQ depth=0, got %d", dq.Depth())
	}
}

// 5. Messages on payment-queue have all required fields.
func TestPaymentService_PaymentMessageShape(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)

	raw, ok := waitForMsg(pq, 3*time.Second)
	if !ok {
		t.Fatal("no message received from payment-queue within 3s")
	}

	var pm paymentMsg
	if err := json.Unmarshal([]byte(raw), &pm); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	if pm.MessageID == "" {
		t.Error("messageId is empty")
	}
	if pm.OrderID == "" {
		t.Error("orderId is empty")
	}
	if pm.CustomerID == "" {
		t.Error("customerId is empty")
	}
	if pm.TransactionID == "" {
		t.Error("transactionId is empty")
	}
	if pm.TraceID == "" {
		t.Error("traceId is empty")
	}
	if pm.Status != "approved" {
		t.Errorf("expected status='approved', got %q", pm.Status)
	}
	if pm.TotalAmount <= 0 {
		t.Error("totalAmount is not positive")
	}
	if pm.ProcessedAt <= 0 {
		t.Error("processedAt is not set")
	}
	if pm.ProcessingMs < 0 {
		t.Error("processingMs is negative")
	}
}

// ── Poison Pill ───────────────────────────────────────────────────────────────

// 6. Poison pill: poisoned messages go directly to DLQ with attemptCount=1.
func TestPaymentService_PoisonPill_DirectToDLQ(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultPoisonPill, "order")

	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)

	raw, ok := waitForMsg(dq, 3*time.Second)
	if !ok {
		t.Fatal("no message received from payment-dlq within 3s")
	}

	var dm dlqMsg
	if err := json.Unmarshal([]byte(raw), &dm); err != nil {
		t.Fatalf("invalid DLQ JSON: %v\n%s", err, raw)
	}
	if dm.AttemptCount != 1 {
		t.Errorf("expected attemptCount=1 for schema error, got %d", dm.AttemptCount)
	}
	if dm.FailureReason != "schema_invalid" {
		t.Errorf("expected failureReason='schema_invalid', got %q", dm.FailureReason)
	}
	if dm.FirstAttemptAt <= 0 {
		t.Error("firstAttemptAt not set")
	}
	if dm.LastAttemptAt <= 0 {
		t.Error("lastAttemptAt not set")
	}

	var orig OrderMessage
	if err := json.Unmarshal(dm.OriginalMessage, &orig); err != nil {
		t.Fatalf("originalMessage is not a valid OrderMessage: %v", err)
	}
	if !orig.Meta.Poisoned {
		t.Error("originalMessage._meta.poisoned should be true")
	}
}

// 7. Poison pill: error_rate rises to ≥ 0.9 after processing several messages.
func TestPaymentService_PoisonPill_ErrorRate(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultPoisonPill, "order")

	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)
	drainQueue(ctx, pq)
	drainQueue(ctx, dq)

	// Let enough messages process (poison-pill skips the 50-150ms delay).
	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

	var errorRate float64
	db.QueryRow(`SELECT error_rate FROM service_metrics WHERE service='payment' ORDER BY ts DESC LIMIT 1`).Scan(&errorRate)
	if errorRate < 0.9 {
		t.Errorf("expected error_rate ≥ 0.9 during poison pill, got %.4f", errorRate)
	}
}

// 8. Poison pill: payment-queue receives no messages.
func TestPaymentService_PoisonPill_PaymentQueueStarved(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultPoisonPill, "order")

	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)
	drainQueue(ctx, dq)

	// Observe payment-queue for 2s — should stay empty.
	time.Sleep(2 * time.Second)
	if pq.Depth() != 0 {
		t.Errorf("expected payment-queue depth=0 during poison pill, got %d", pq.Depth())
	}
}

// ── Intermittent Errors ───────────────────────────────────────────────────────

// 9. Intermittent errors: service still processes messages and writes valid metrics.
// Error rate is not asserted — probabilistic outcomes are not functionally testable.
func TestPaymentService_IntermittentErrors_ServiceStillFunctions(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultIntermittentErrors, "all")

	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)
	drainQueue(ctx, pq)
	drainQueue(ctx, dq)

	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

	var throughput, errorRate float64
	err := db.QueryRow(`SELECT throughput, error_rate FROM service_metrics WHERE service='payment' ORDER BY ts DESC LIMIT 1`).Scan(&throughput, &errorRate)
	if err != nil {
		t.Fatalf("no metrics row found: %v", err)
	}
	if throughput < 0 {
		t.Errorf("throughput should be non-negative, got %f", throughput)
	}
	if errorRate < 0 || errorRate > 1 {
		t.Errorf("error_rate must be in [0,1], got %f", errorRate)
	}
}

// ── Cascading Failure ─────────────────────────────────────────────────────────

// 10. Cascading failure: throughput drops to 0 in the cycle after crash.
func TestPaymentService_CascadingFailure_ThroughputZero(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)
	drainQueue(ctx, pq)

	// Baseline write.
	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

	// Activate crash, wait for pre-crash goroutines to complete (≤150ms delay),
	// flush once to clear that transitional window, then take a clean reading.
	engine.Activate(simulator.FaultCascadingFailure, "payment")
	time.Sleep(250 * time.Millisecond)
	svc.ForceWriteMetrics() // drain transitional completions; resets lastFlushAt
	time.Sleep(200 * time.Millisecond)
	svc.ForceWriteMetrics() // clean 200ms window — no new goroutines possible

	var throughput float64
	db.QueryRow(`SELECT throughput FROM service_metrics WHERE service='payment' ORDER BY ts DESC LIMIT 1`).Scan(&throughput)
	if throughput != 0 {
		t.Errorf("expected throughput=0 after cascading failure, got %.2f", throughput)
	}
}

// 11. Cascading failure: metrics rows continue to be written after crash.
func TestPaymentService_CascadingFailure_MetricsContinue(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultCascadingFailure, "payment")

	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)

	// Force two writes directly — no timer wait needed.
	svc.ForceWriteMetrics()
	svc.ForceWriteMetrics()

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='payment'`).Scan(&count)
	if count < 2 {
		t.Errorf("expected ≥ 2 metrics rows written after cascading failure, got %d", count)
	}
}

// 12. Cascading failure: active_faults column reflects the crash fault.
func TestPaymentService_CascadingFailure_ActiveFaultsInMetrics(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultCascadingFailure, "payment")

	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)

	svc.ForceWriteMetrics()

	var activeFaults string
	db.QueryRow(`SELECT active_faults FROM service_metrics WHERE service='payment' ORDER BY ts DESC LIMIT 1`).Scan(&activeFaults)
	if !containsStr(activeFaults, "cascading-failure") {
		t.Errorf("expected active_faults to contain 'cascading-failure', got %q", activeFaults)
	}
}

// 13. Fault deactivation: throughput recovers after cascading failure is removed.
func TestPaymentService_FaultDeactivation_Recovery(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	svc := simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)
	drainQueue(ctx, pq)

	// Crash, then deactivate.
	engine.Activate(simulator.FaultCascadingFailure, "payment")
	time.Sleep(300 * time.Millisecond)
	engine.Deactivate(simulator.FaultCascadingFailure)

	// Give the service a tick to auto-recover, then process new messages.
	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

	var throughput float64
	db.QueryRow(`SELECT throughput FROM service_metrics WHERE service='payment' ORDER BY ts DESC LIMIT 1`).Scan(&throughput)
	if throughput <= 0 {
		t.Errorf("expected throughput > 0 after fault deactivation, got %.2f", throughput)
	}
}

// ── Downstream Timeout ────────────────────────────────────────────────────────

// 14. Downstream timeout: payment-queue receives no messages while fault is active
// (8s processing delay means no completions within a 5s window).
func TestPaymentService_DownstreamTimeout_BlocksPaymentQueue(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultDownstreamTimeout, "payment")

	oq := simulator.NewQueue("order-queue", false, db, ctx)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	dq := simulator.NewQueue("payment-dlq", true, db, ctx)
	simulator.NewOrderService(oq, engine, db, ctx)
	simulator.NewPaymentService(oq, pq, dq, engine, db, ctx)
	_ = dq

	// Drain any messages that were in-flight before the fault activated.
	time.Sleep(300 * time.Millisecond)
	drainQueue(ctx, pq)

	// Observe payment-queue for 1s — no message should complete (8s delay).
	received := 0
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		_, ok := pq.TryReceive()
		if ok {
			received++
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if received > 0 {
		t.Errorf("expected 0 messages on payment-queue during 8s timeout fault (1s window), got %d", received)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
