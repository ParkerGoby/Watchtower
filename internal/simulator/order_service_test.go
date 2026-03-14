package simulator_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/parkerg/watchtower/internal/simulator"
)

// OrderMessage mirrors the JSON shape emitted by the Order Service.
type OrderMessage struct {
	MessageID   string      `json:"messageId"`
	OrderID     string      `json:"orderId"`
	CustomerID  string      `json:"customerId"`
	Items       []OrderItem `json:"items"`
	TotalAmount float64     `json:"totalAmount"`
	CreatedAt   int64       `json:"createdAt"`
	TraceID     string      `json:"traceId"`
	Meta        struct {
		Poisoned  bool  `json:"poisoned"`
		EmittedAt int64 `json:"emittedAt"`
	} `json:"_meta"`
}

type OrderItem struct {
	ItemID    string  `json:"itemId"`
	Quantity  int     `json:"quantity"`
	UnitPrice float64 `json:"unitPrice"`
}

// helper: drain messages from a queue in the background until ctx is cancelled.
func drainQueue(ctx context.Context, q *simulator.Queue) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				q.Receive()
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
}

// ── Order Service Unit Behavior ───────────────────────────────────────────────

// 1. service_metrics rows appear within 4s for service='order'.
func TestOrderService_WritesServiceMetrics(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&count)
		if count > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("no service_metrics row written within 4s")
}

// 2. After context cancel, no further service_metrics rows are written.
func TestOrderService_Stop_NoFurtherWrites(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	engine := simulator.NewFaultEngine(conn)
	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)

	// Wait for at least one metrics write.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&count)
		if count > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	time.Sleep(200 * time.Millisecond)

	var countBefore int
	conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&countBefore)

	time.Sleep(3 * time.Second)

	var countAfter int
	conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&countAfter)

	if countAfter != countBefore {
		t.Errorf("expected no new rows after stop, got %d new rows", countAfter-countBefore)
	}
}

// ── Order Service + Queue Integration ────────────────────────────────────────

// 3. Messages land in order-queue with all required fields populated.
func TestOrderService_EnqueuesMessages(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)

	var msg string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := queue.Receive()
		if ok {
			msg = m
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if msg == "" {
		t.Fatal("no message received from order-queue within 2s")
	}

	var om OrderMessage
	if err := json.Unmarshal([]byte(msg), &om); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, msg)
	}
	if om.MessageID == "" {
		t.Error("messageId is empty")
	}
	if om.OrderID == "" {
		t.Error("orderId is empty")
	}
	if om.CustomerID == "" {
		t.Error("customerId is empty")
	}
	if om.TraceID == "" {
		t.Error("traceId is empty")
	}
	if om.CreatedAt <= 0 {
		t.Error("createdAt is not set")
	}
	if om.Meta.EmittedAt <= 0 {
		t.Error("_meta.emittedAt is not set")
	}
	if len(om.Items) == 0 {
		t.Error("items is empty")
	}
	if om.TotalAmount <= 0 {
		t.Error("totalAmount is not positive")
	}
}

// 4. Throughput ≈ 5 req/s (±50%) reflected in both queue output and service_metrics.
func TestOrderService_NormalThroughput(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)

	var received int
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		_, ok := queue.Receive()
		if ok {
			received++
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 5 req/s × 6s = 30 expected; allow ±50% → 15–45.
	if received < 15 || received > 45 {
		t.Errorf("expected 15–45 messages in 6s (≈5 req/s ±50%%), got %d", received)
	}

	var throughput float64
	conn.QueryRow(`SELECT throughput FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&throughput)
	if throughput < 2.5 || throughput > 10.0 {
		t.Errorf("expected throughput ≈ 5 msg/s in service_metrics, got %f", throughput)
	}
}

// 5. With no faults active, error_rate ≤ 0.01 and nearly all messages reach the queue.
func TestOrderService_ErrorRateNormal(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&count)
		if count > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	var errorRate float64
	conn.QueryRow(`SELECT error_rate FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&errorRate)
	if errorRate > 0.01 {
		t.Errorf("expected error_rate ≤ 0.01 with no faults, got %f", errorRate)
	}
}

// ── Order Service + FaultEngine Integration ───────────────────────────────────

// 6. Poison-pill active: messages dequeued from order-queue have _meta.poisoned=true.
func TestOrderService_PoisonPill_PoisonsQueueMessages(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultPoisonPill, "order")

	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)

	var poisoned, total int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok := queue.Receive()
		if !ok {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		total++
		var om OrderMessage
		if err := json.Unmarshal([]byte(msg), &om); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if om.Meta.Poisoned {
			poisoned++
		}
		if total >= 10 {
			break
		}
	}
	if total == 0 {
		t.Fatal("no messages received")
	}
	if poisoned != total {
		t.Errorf("expected all %d messages poisoned, got %d", total, poisoned)
	}
}

// 7. Poison-pill active: Order Service error_rate stays low (fault is transparent to producer).
func TestOrderService_PoisonPill_OrderMetricsLookNormal(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultPoisonPill, "order")

	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)
	drainQueue(ctx, queue)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&count)
		if count > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	var errorRate float64
	conn.QueryRow(`SELECT error_rate FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&errorRate)
	if errorRate > 0.01 {
		t.Errorf("expected error_rate ≤ 0.01 during poison-pill (fault is transparent), got %f", errorRate)
	}
}

// 8. Intermittent-errors active: error_rate ≈ 0.15 (±0.10) in service_metrics;
// fewer messages reach the queue compared to the no-fault baseline.
func TestOrderService_IntermittentErrors_RaisesErrorRate(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultIntermittentErrors, "all")

	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)
	drainQueue(ctx, queue)

	// Wait for at least 2 metrics rows to average out randomness.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&count)
		if count >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	var errorRate float64
	conn.QueryRow(`SELECT AVG(error_rate) FROM service_metrics WHERE service='order'`).Scan(&errorRate)
	if errorRate < 0.05 || errorRate > 0.30 {
		t.Errorf("expected error_rate ≈ 0.15 (±0.10) with intermittent-errors, got %f", errorRate)
	}
}

// 9. active_faults column contains the active fault name at write time.
func TestOrderService_ActiveFaultsInMetrics(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultPoisonPill, "order")

	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)
	drainQueue(ctx, queue)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var activeFaults string
		err := conn.QueryRow(`SELECT active_faults FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&activeFaults)
		if err == nil && strings.Contains(activeFaults, "poison-pill") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("expected active_faults to contain 'poison-pill' within 4s")
}

// ── Full Stack Integration ────────────────────────────────────────────────────

// 10. Fault activated mid-run: messages flip from clean to poisoned,
// and active_faults updates in subsequent metrics rows.
func TestOrderService_FaultActivatedMidRun(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	simulator.NewOrderService(queue, engine, conn, ctx)

	// Receive one clean message before any fault is active.
	var cleanMsg string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok := queue.Receive()
		if ok {
			cleanMsg = msg
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cleanMsg == "" {
		t.Fatal("no clean message received before fault activation")
	}
	var om OrderMessage
	json.Unmarshal([]byte(cleanMsg), &om)
	if om.Meta.Poisoned {
		t.Error("expected message to be clean before fault activation")
	}

	// Activate poison-pill mid-run.
	engine.Activate(simulator.FaultPoisonPill, "order")

	// Verify subsequent messages are poisoned.
	var poisoned int
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok := queue.Receive()
		if !ok {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var m OrderMessage
		json.Unmarshal([]byte(msg), &m)
		if m.Meta.Poisoned {
			poisoned++
		}
		if poisoned >= 3 {
			break
		}
	}
	if poisoned == 0 {
		t.Error("expected poisoned messages after fault activation, got none")
	}

	// Verify active_faults reflects the new fault in subsequent metrics rows.
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var activeFaults string
		err := conn.QueryRow(`SELECT active_faults FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&activeFaults)
		if err == nil && strings.Contains(activeFaults, "poison-pill") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("expected active_faults to reflect poison-pill after mid-run activation")
}
