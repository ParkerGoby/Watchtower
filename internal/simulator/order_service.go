package simulator

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	mrand "math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// orderMessage is the JSON payload enqueued onto order-queue.
type orderMessage struct {
	MessageID   string       `json:"messageId"`
	OrderID     string       `json:"orderId"`
	CustomerID  string       `json:"customerId"`
	Items       []orderItem  `json:"items"`
	TotalAmount float64      `json:"totalAmount"`
	CreatedAt   int64        `json:"createdAt"`
	TraceID     string       `json:"traceId"`
	Meta        orderMsgMeta `json:"_meta"`
}

type orderItem struct {
	ItemID    string  `json:"itemId"`
	Quantity  int     `json:"quantity"`
	UnitPrice float64 `json:"unitPrice"`
}

type orderMsgMeta struct {
	Poisoned  bool  `json:"poisoned"`
	EmittedAt int64 `json:"emittedAt"`
}

// OrderService simulates the order entry point of the e-commerce pipeline.
// It generates synthetic order messages at ~5 req/s and enqueues them onto
// order-queue. It is safe for concurrent use and lifecycle is managed via
// the provided context.
type OrderService struct {
	queue  *Queue
	engine *FaultEngine
	db     *sql.DB
	ctx    context.Context

	mu          sync.Mutex
	enqueued    int
	errors      int
	totalReqs   int
	latencies   []float64 // ms
	lastFlushAt time.Time
}

// NewOrderService creates an OrderService and starts its background goroutines.
func NewOrderService(queue *Queue, engine *FaultEngine, db *sql.DB, ctx context.Context) *OrderService {
	s := &OrderService{
		queue:       queue,
		engine:      engine,
		db:          db,
		ctx:         ctx,
		lastFlushAt: time.Now(),
	}
	go s.runTicker()
	go s.runMetricsWriter()
	return s
}

// ForceWriteMetrics immediately flushes accumulated counters to service_metrics
// using the actual elapsed time since the last flush. Intended for tests so
// they don't have to wait for the background timer to fire.
func (s *OrderService) ForceWriteMetrics() {
	now := time.Now()
	s.mu.Lock()
	elapsed := now.Sub(s.lastFlushAt)
	s.mu.Unlock()
	if elapsed < time.Millisecond {
		elapsed = time.Millisecond
	}
	s.writeMetrics(elapsed)
}

// runTicker fires a processing tick every 200ms (~5 req/s).
func (s *OrderService) runTicker() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.processTick()
		case <-s.ctx.Done():
			return
		}
	}
}

// processTick generates one order message and enqueues it (or drops it on error).
func (s *OrderService) processTick() {
	start := time.Now()
	fctx := s.engine.GetFaultContext("order")

	s.mu.Lock()
	s.totalReqs++
	s.mu.Unlock()

	// Intermittent errors: randomly discard the message without enqueuing.
	if fctx.ErrorProbability > 0 && mrand.Float64() < fctx.ErrorProbability {
		s.mu.Lock()
		s.errors++
		s.mu.Unlock()
		return
	}

	poisoned := s.engine.IsFaultActive(FaultPoisonPill)

	now := time.Now().UnixMilli()
	msg := orderMessage{
		MessageID:  newUUID(),
		OrderID:    newUUID(),
		CustomerID: newUUID(),
		Items:      []orderItem{randomItem()},
		CreatedAt:  now,
		TraceID:    newUUID(),
		Meta: orderMsgMeta{
			Poisoned:  poisoned,
			EmittedAt: now,
		},
	}
	msg.TotalAmount = msg.Items[0].UnitPrice * float64(msg.Items[0].Quantity)

	payload, err := json.Marshal(msg)
	if err != nil {
		s.mu.Lock()
		s.errors++
		s.mu.Unlock()
		return
	}

	s.queue.Send(string(payload))

	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	s.mu.Lock()
	s.enqueued++
	s.latencies = append(s.latencies, latencyMs)
	s.mu.Unlock()
}

// runMetricsWriter writes a service_metrics row every 2–3s (jittered).
func (s *OrderService) runMetricsWriter() {
	for {
		delay := time.Duration(2000+mrand.Intn(1000)) * time.Millisecond
		select {
		case <-time.After(delay):
			s.writeMetrics(delay)
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *OrderService) writeMetrics(elapsed time.Duration) {
	s.mu.Lock()
	enqueued := s.enqueued
	errors := s.errors
	totalReqs := s.totalReqs
	latencies := s.latencies
	s.enqueued = 0
	s.errors = 0
	s.totalReqs = 0
	s.latencies = nil
	s.lastFlushAt = time.Now()
	s.mu.Unlock()

	elapsedSec := elapsed.Seconds()
	throughput := float64(enqueued) / elapsedSec

	var errorRate float64
	if totalReqs > 0 {
		errorRate = float64(errors) / float64(totalReqs)
	}

	p50, p99 := percentiles(latencies)

	activeFaults := activeFaultNames(s.engine)

	ts := time.Now().UnixMilli()
	_, _ = s.db.Exec(
		`INSERT INTO service_metrics (ts, service, throughput, error_rate, p50_latency, p99_latency, active_faults)
		 VALUES (?, 'order', ?, ?, ?, ?, ?)`,
		ts, throughput, errorRate, p50, p99, activeFaults,
	)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var itemCatalog = []struct {
	id    string
	price float64
}{
	{"item-widget-a", 9.99},
	{"item-gadget-b", 24.99},
	{"item-doohickey-c", 4.99},
	{"item-thingamajig-d", 49.99},
	{"item-whatsit-e", 14.99},
}

func randomItem() orderItem {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(itemCatalog))))
	entry := itemCatalog[n.Int64()]
	qty, _ := rand.Int(rand.Reader, big.NewInt(4))
	return orderItem{
		ItemID:    entry.id,
		Quantity:  int(qty.Int64()) + 1,
		UnitPrice: entry.price,
	}
}

func percentiles(latencies []float64) (p50, p99 float64) {
	if len(latencies) == 0 {
		return 0, 0
	}
	sorted := make([]float64, len(latencies))
	copy(sorted, latencies)
	sort.Float64s(sorted)
	p50 = sorted[int(float64(len(sorted))*0.50)]
	p99 = sorted[int(float64(len(sorted))*0.99)]
	return p50, p99
}

func activeFaultNames(engine *FaultEngine) string {
	faults := engine.GetActiveFaults()
	names := make([]string, len(faults))
	for i, f := range faults {
		names[i] = string(f.Type)
	}
	return strings.Join(names, ",")
}
