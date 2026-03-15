package simulator_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/parkerg/monitower/internal/simulator"
)

// notifDLQMsg mirrors the DLQ envelope written by the Notification Service.
type notifDLQMsg struct {
	OriginalMessage  json.RawMessage `json:"originalMessage"`
	FailureReason    string          `json:"failureReason"`
	AttemptCount     int             `json:"attemptCount"`
	FirstAttemptAt   int64           `json:"firstAttemptAt"`
	LastAttemptAt    int64           `json:"lastAttemptAt"`
	NotificationType string          `json:"notificationType"`
}

// ── Service Metrics ───────────────────────────────────────────────────────────

// 1. ForceWriteMetrics writes a service_metrics row immediately.
func TestNotificationService_WritesMetrics(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)
	svc := simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	svc.ForceWriteMetrics()

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='notification'`).Scan(&count)
	if count == 0 {
		t.Error("expected at least one service_metrics row after ForceWriteMetrics")
	}
}

// 2. After context cancel, no further metrics rows are written.
func TestNotificationService_Stop_NoFurtherWrites(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	engine := simulator.NewFaultEngine(db)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)
	svc := simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	svc.ForceWriteMetrics()
	cancel()
	time.Sleep(100 * time.Millisecond)

	var countBefore int
	db.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='notification'`).Scan(&countBefore)

	time.Sleep(100 * time.Millisecond)

	var countAfter int
	db.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='notification'`).Scan(&countAfter)
	if countAfter != countBefore {
		t.Errorf("expected no new rows after stop, got %d new rows", countAfter-countBefore)
	}
}

// ── Happy Path ────────────────────────────────────────────────────────────────

// 3. Approved messages are processed; throughput > 0, error_rate = 0, DLQ depth = 0.
func TestNotificationService_ApprovedMessages_Throughput(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)

	for i := 0; i < 20; i++ {
		pq.Send(makePaymentMsg("approved"))
	}

	svc := simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	time.Sleep(1 * time.Second)
	svc.ForceWriteMetrics()

	var throughput, errorRate float64
	err := db.QueryRow(
		`SELECT throughput, error_rate FROM service_metrics WHERE service='notification' ORDER BY ts DESC LIMIT 1`,
	).Scan(&throughput, &errorRate)
	if err != nil {
		t.Fatalf("no metrics row found: %v", err)
	}
	if throughput <= 0 {
		t.Errorf("expected throughput > 0 for approved messages, got %.4f", throughput)
	}
	if errorRate != 0 {
		t.Errorf("expected error_rate = 0 with no faults, got %.4f", errorRate)
	}
	if dlq.Depth() != 0 {
		t.Errorf("expected notification-dlq depth = 0 in happy path, got %d", dlq.Depth())
	}
}

// 4. Declined messages are processed the same as approved; throughput > 0, error_rate = 0.
func TestNotificationService_DeclinedMessages_CountAsThroughput(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)

	for i := 0; i < 10; i++ {
		pq.Send(makePaymentMsg("declined"))
	}

	svc := simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

	var throughput, errorRate float64
	err := db.QueryRow(
		`SELECT throughput, error_rate FROM service_metrics WHERE service='notification' ORDER BY ts DESC LIMIT 1`,
	).Scan(&throughput, &errorRate)
	if err != nil {
		t.Fatalf("no metrics row found: %v", err)
	}
	if throughput <= 0 {
		t.Errorf("expected throughput > 0 for declined messages, got %.4f", throughput)
	}
	if errorRate != 0 {
		t.Errorf("expected error_rate = 0 for declined messages with no faults, got %.4f", errorRate)
	}
}

// ── DLQ Routing ───────────────────────────────────────────────────────────────

// 5. When errors exhaust all retries, a correctly-shaped DLQ message is produced.
// Uses FaultNotificationFailure (100% error probability for notification) for determinism.
func TestNotificationService_DLQRouting_MessageShape(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultNotificationFailure, "notification")

	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)

	for i := 0; i < 5; i++ {
		pq.Send(makePaymentMsg("approved"))
	}

	simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	raw, ok := waitForMsg(dlq, 5*time.Second)
	if !ok {
		t.Fatal("no DLQ message received within 5s")
	}

	var dm notifDLQMsg
	if err := json.Unmarshal([]byte(raw), &dm); err != nil {
		t.Fatalf("invalid DLQ JSON: %v\n%s", err, raw)
	}
	if dm.AttemptCount != 2 {
		t.Errorf("expected attemptCount=2 (max retries), got %d", dm.AttemptCount)
	}
	if dm.FailureReason != "processing_error" {
		t.Errorf("expected failureReason='processing_error', got %q", dm.FailureReason)
	}
	if dm.NotificationType != "confirmation" {
		t.Errorf("expected notificationType='confirmation' for approved message, got %q", dm.NotificationType)
	}
	if dm.FirstAttemptAt <= 0 {
		t.Error("firstAttemptAt must be set")
	}
	if dm.LastAttemptAt < dm.FirstAttemptAt {
		t.Error("lastAttemptAt must be >= firstAttemptAt")
	}

	var orig paymentMsg
	if err := json.Unmarshal(dm.OriginalMessage, &orig); err != nil {
		t.Fatalf("originalMessage is not a valid paymentMessage: %v", err)
	}
}

// 6. Declined messages that exhaust retries produce notificationType='declined' in the DLQ.
func TestNotificationService_DLQRouting_DeclinedNotificationType(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultNotificationFailure, "notification")

	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)

	pq.Send(makePaymentMsg("declined"))

	simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	raw, ok := waitForMsg(dlq, 5*time.Second)
	if !ok {
		t.Fatal("no DLQ message received within 5s")
	}

	var dm notifDLQMsg
	if err := json.Unmarshal([]byte(raw), &dm); err != nil {
		t.Fatalf("invalid DLQ JSON: %v", err)
	}
	if dm.NotificationType != "declined" {
		t.Errorf("expected notificationType='declined', got %q", dm.NotificationType)
	}
}

// ── Fan-Out ───────────────────────────────────────────────────────────────────

// 7. Fan-out: Notification and Fulfillment maintain independent read pointers.
// Each receives a copy of every message — they do not compete for the same messages.
func TestNotificationService_FanOut_IndependentPointers(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)

	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)

	const msgCount = 10
	for i := 0; i < msgCount; i++ {
		pq.Send(makePaymentMsg("approved"))
	}

	fulfillmentSvc := simulator.NewFulfillmentService(pq, engine, db, ctx)
	notifSvc := simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	time.Sleep(2 * time.Second)
	fulfillmentSvc.ForceWriteMetrics()
	notifSvc.ForceWriteMetrics()

	// Both subscriptions should have been drained by their respective consumers.
	if pq.Depth() > 0 {
		t.Errorf("payment-queue (Fulfillment) not fully drained: depth=%d", pq.Depth())
	}
	if notifSub.Depth() > 0 {
		t.Errorf("notification subscription not fully drained: depth=%d", notifSub.Depth())
	}

	// Both services should have non-zero throughput, confirming each got its own copies.
	var fulfillThroughput, notifThroughput float64
	db.QueryRow(
		`SELECT throughput FROM service_metrics WHERE service='fulfillment' ORDER BY ts DESC LIMIT 1`,
	).Scan(&fulfillThroughput)
	db.QueryRow(
		`SELECT throughput FROM service_metrics WHERE service='notification' ORDER BY ts DESC LIMIT 1`,
	).Scan(&notifThroughput)

	if fulfillThroughput <= 0 {
		t.Errorf("Fulfillment should have processed messages, throughput=%.4f", fulfillThroughput)
	}
	if notifThroughput <= 0 {
		t.Errorf("Notification should have processed messages, throughput=%.4f", notifThroughput)
	}
}

// ── Starvation ────────────────────────────────────────────────────────────────

// 8. Empty subscription queue produces throughput=0, error_rate=0, DLQ depth=0.
func TestNotificationService_Starvation_ZeroThroughput(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)
	svc := simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	time.Sleep(300 * time.Millisecond)
	svc.ForceWriteMetrics()

	var throughput, errorRate float64
	err := db.QueryRow(
		`SELECT throughput, error_rate FROM service_metrics WHERE service='notification' ORDER BY ts DESC LIMIT 1`,
	).Scan(&throughput, &errorRate)
	if err != nil {
		t.Fatalf("no metrics row: %v", err)
	}
	if throughput != 0 {
		t.Errorf("expected throughput=0 with empty queue (starvation), got %.4f", throughput)
	}
	if errorRate != 0 {
		t.Errorf("expected error_rate=0 with empty queue (starvation), got %.4f", errorRate)
	}
	if dlq.Depth() != 0 {
		t.Errorf("expected DLQ depth=0 during starvation, got %d", dlq.Depth())
	}
}

// ── Fault Behavior ────────────────────────────────────────────────────────────

// 9. Intermittent errors fault: service writes valid metrics in [0,1] ranges.
func TestNotificationService_IntermittentErrors_MetricsValid(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultIntermittentErrors, "all")

	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)

	for i := 0; i < 30; i++ {
		pq.Send(makePaymentMsg("approved"))
	}

	svc := simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	time.Sleep(1 * time.Second)
	svc.ForceWriteMetrics()

	var throughput, errorRate float64
	err := db.QueryRow(
		`SELECT throughput, error_rate FROM service_metrics WHERE service='notification' ORDER BY ts DESC LIMIT 1`,
	).Scan(&throughput, &errorRate)
	if err != nil {
		t.Fatalf("no metrics row: %v", err)
	}
	if throughput < 0 {
		t.Errorf("throughput must be non-negative, got %.4f", throughput)
	}
	if errorRate < 0 || errorRate > 1 {
		t.Errorf("error_rate must be in [0,1], got %.4f", errorRate)
	}
}

// 10. active_faults column reflects active faults at write time.
func TestNotificationService_ActiveFaultsInMetrics(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultIntermittentErrors, "all")

	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)
	svc := simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	svc.ForceWriteMetrics()

	var activeFaults string
	db.QueryRow(
		`SELECT active_faults FROM service_metrics WHERE service='notification' ORDER BY ts DESC LIMIT 1`,
	).Scan(&activeFaults)
	if !containsStr(activeFaults, "intermittent-errors") {
		t.Errorf("expected active_faults to contain 'intermittent-errors', got %q", activeFaults)
	}
}

// 11. DLQ messages accumulate and are never drained (no consumer).
// This validates the silent failure pattern: DLQ only grows, never shrinks.
func TestNotificationService_DLQNeverDrained(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(db)
	engine.Activate(simulator.FaultNotificationFailure, "notification")

	pq := simulator.NewQueue("payment-queue", false, db, ctx)
	notifSub := pq.Subscribe("notification-payment-sub", false, db, ctx)
	dlq := simulator.NewQueue("notification-dlq", true, db, ctx)

	for i := 0; i < 10; i++ {
		pq.Send(makePaymentMsg("approved"))
	}

	simulator.NewNotificationService(notifSub, dlq, engine, db, ctx)

	time.Sleep(3 * time.Second)
	depthAfterRun := dlq.Depth()
	if depthAfterRun == 0 {
		t.Error("expected notification-dlq to have accumulated messages; it remains empty")
	}

	// Wait another second without sending new messages: depth must not decrease.
	time.Sleep(1 * time.Second)
	depthAfterIdle := dlq.Depth()
	if depthAfterIdle < depthAfterRun {
		t.Errorf("DLQ depth decreased from %d to %d — something is consuming the DLQ unexpectedly",
			depthAfterRun, depthAfterIdle)
	}
}
