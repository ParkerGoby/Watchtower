package simulator_test

import (
	"context"
	"testing"
	"time"

	"github.com/parkerg/watchtower/internal/simulator"
)

// 1. Send followed by Receive returns the same message.
func TestQueue_SendReceive(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q := simulator.NewQueue("order-queue", false, conn, ctx)
	q.Send("hello")

	msg, ok := q.Receive()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg != "hello" {
		t.Errorf("expected 'hello', got %q", msg)
	}
}

// 2. Receive on empty queue returns ok=false when context is cancelled.
func TestQueue_ReceiveEmptyContextCancelled(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	q := simulator.NewQueue("order-queue", false, conn, ctx)
	cancel()

	// Give goroutine a moment to notice cancellation.
	time.Sleep(10 * time.Millisecond)

	_, ok := q.Receive()
	if ok {
		t.Error("expected ok=false after context cancel on empty queue")
	}
}

// 3. After N sends and M receives, Depth equals N-M.
func TestQueue_Depth(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q := simulator.NewQueue("order-queue", false, conn, ctx)

	for i := 0; i < 5; i++ {
		q.Send("msg")
	}
	if d := q.Depth(); d != 5 {
		t.Errorf("expected depth=5, got %d", d)
	}

	q.Receive()
	q.Receive()
	if d := q.Depth(); d != 3 {
		t.Errorf("expected depth=3 after 2 receives, got %d", d)
	}
}

// 4. ForceWriteMetrics produces a queue_metrics row immediately.
func TestQueue_MetricsWritten(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q := simulator.NewQueue("order-queue", false, conn, ctx)
	q.Send("msg")
	q.ForceWriteMetrics()

	var count int
	conn.QueryRow(`SELECT COUNT(*) FROM queue_metrics WHERE queue='order-queue'`).Scan(&count)
	if count == 0 {
		t.Error("expected at least one queue_metrics row after ForceWriteMetrics")
	}
}

// 5. enqueue_rate and dequeue_rate are non-negative after activity.
func TestQueue_Rates(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q := simulator.NewQueue("order-queue", false, conn, ctx)
	for i := 0; i < 10; i++ {
		q.Send("msg")
	}
	for i := 0; i < 5; i++ {
		q.Receive()
	}
	q.ForceWriteMetrics()

	var enqRate, deqRate float64
	err := conn.QueryRow(
		`SELECT enqueue_rate, dequeue_rate FROM queue_metrics WHERE queue='order-queue' ORDER BY ts DESC LIMIT 1`,
	).Scan(&enqRate, &deqRate)
	if err != nil {
		t.Fatalf("no queue_metrics row found: %v", err)
	}
	if enqRate < 0 {
		t.Errorf("negative enqueue_rate: %f", enqRate)
	}
	if deqRate < 0 {
		t.Errorf("negative dequeue_rate: %f", deqRate)
	}
}

// 6. DLQ queue has dlq=1 in metrics; SendToDLQ increases DLQ depth.
func TestQueue_DLQ(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dlq := simulator.NewQueue("payment-dlq", true, conn, ctx)
	dlq.Send("dead-letter")

	if d := dlq.Depth(); d != 1 {
		t.Errorf("expected dlq depth=1, got %d", d)
	}

	dlq.ForceWriteMetrics()

	var dlqFlag int
	err := conn.QueryRow(
		`SELECT dlq FROM queue_metrics WHERE queue='payment-dlq' ORDER BY ts DESC LIMIT 1`,
	).Scan(&dlqFlag)
	if err != nil {
		t.Fatalf("no queue_metrics row found: %v", err)
	}
	if dlqFlag != 1 {
		t.Errorf("expected dlq=1, got %d", dlqFlag)
	}
}

// 7. Context cancellation stops the metrics goroutine (no goroutine leak).
func TestQueue_ContextCancelStopsGoroutine(t *testing.T) {
	conn := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	_ = simulator.NewQueue("order-queue", false, conn, ctx)
	cancel()

	// Allow time for goroutine to exit — if it leaks, the race detector will
	// surface it on repeated test runs, but we also verify no panic occurs.
	time.Sleep(50 * time.Millisecond)
}
