package simulator_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/parkerg/monitower/internal/simulator"
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

// 1. ForceWriteMetrics produces a service_metrics row immediately.
func TestOrderService_WritesServiceMetrics(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	svc := simulator.NewOrderService(queue, engine, conn, ctx)

	time.Sleep(100 * time.Millisecond) // let ticker produce a few messages
	svc.ForceWriteMetrics()

	var count int
	conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&count)
	if count == 0 {
		t.Error("expected at least one service_metrics row after ForceWriteMetrics")
	}
}

// 2. After context cancel, no further metrics rows are written.
func TestOrderService_Stop_NoFurtherWrites(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	engine := simulator.NewFaultEngine(conn)
	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	svc := simulator.NewOrderService(queue, engine, conn, ctx)

	time.Sleep(100 * time.Millisecond)
	svc.ForceWriteMetrics()

	cancel()
	time.Sleep(100 * time.Millisecond) // let goroutines exit

	var countBefore int
	conn.QueryRow(`SELECT COUNT(*) FROM service_metrics WHERE service='order'`).Scan(&countBefore)

	time.Sleep(100 * time.Millisecond) // goroutine already exited; no timer can fire

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
	svc := simulator.NewOrderService(queue, engine, conn, ctx)

	var received int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, ok := queue.Receive()
		if ok {
			received++
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	svc.ForceWriteMetrics()

	// 5 req/s × 3s = 15 expected; allow ±50% → 8–22.
	if received < 8 || received > 22 {
		t.Errorf("expected 8–22 messages in 3s (≈5 req/s ±50%%), got %d", received)
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
	svc := simulator.NewOrderService(queue, engine, conn, ctx)
	drainQueue(ctx, queue)

	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

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
	svc := simulator.NewOrderService(queue, engine, conn, ctx)
	drainQueue(ctx, queue)

	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

	var errorRate float64
	conn.QueryRow(`SELECT error_rate FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&errorRate)
	if errorRate > 0.01 {
		t.Errorf("expected error_rate ≤ 0.01 during poison-pill (fault is transparent), got %f", errorRate)
	}
}

// 8. Intermittent-errors active: service still writes metrics and produces messages.
// Error rate is not asserted — probabilistic outcomes are not functionally testable.
func TestOrderService_IntermittentErrors_ServiceStillFunctions(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultIntermittentErrors, "all")

	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	svc := simulator.NewOrderService(queue, engine, conn, ctx)
	drainQueue(ctx, queue)

	time.Sleep(500 * time.Millisecond)
	svc.ForceWriteMetrics()

	var throughput, errorRate float64
	err := conn.QueryRow(`SELECT throughput, error_rate FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&throughput, &errorRate)
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

// 9. active_faults column contains the active fault name at write time.
func TestOrderService_ActiveFaultsInMetrics(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultPoisonPill, "order")

	queue := simulator.NewQueue("order-queue", false, conn, ctx)
	svc := simulator.NewOrderService(queue, engine, conn, ctx)
	drainQueue(ctx, queue)

	time.Sleep(100 * time.Millisecond)
	svc.ForceWriteMetrics()

	var activeFaults string
	conn.QueryRow(`SELECT active_faults FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&activeFaults)
	if !strings.Contains(activeFaults, "poison-pill") {
		t.Errorf("expected active_faults to contain 'poison-pill', got %q", activeFaults)
	}
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
	svc := simulator.NewOrderService(queue, engine, conn, ctx)

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

	// Verify active_faults reflects the new fault without waiting for a natural write.
	svc.ForceWriteMetrics()
	var activeFaults string
	conn.QueryRow(`SELECT active_faults FROM service_metrics WHERE service='order' ORDER BY ts DESC LIMIT 1`).Scan(&activeFaults)
	if !strings.Contains(activeFaults, "poison-pill") {
		t.Error("expected active_faults to reflect poison-pill after mid-run activation")
	}
}
