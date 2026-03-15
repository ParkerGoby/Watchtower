package simulator

import (
	"context"
	"database/sql"
	"encoding/json"
	mrand "math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// paymentMessage is the JSON payload produced onto payment-queue on success.
type paymentMessage struct {
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

// dlqMessage wraps a failed OrderMessage in a dead-letter envelope.
type dlqMessage struct {
	OriginalMessage json.RawMessage `json:"originalMessage"`
	FailureReason   string          `json:"failureReason"`
	AttemptCount    int             `json:"attemptCount"`
	FirstAttemptAt  int64           `json:"firstAttemptAt"`
	LastAttemptAt   int64           `json:"lastAttemptAt"`
}

type inFlightEntry struct {
	attempts       int
	firstAttemptAt int64
}

// PaymentService consumes from order-queue, processes payments, and routes
// results to payment-queue (success) or payment-dlq (exhausted retries /
// schema errors). It is safe for concurrent use; lifecycle is managed via ctx.
type PaymentService struct {
	orderQueue   *Queue
	paymentQueue *Queue
	dlqQueue     *Queue
	engine       *FaultEngine
	db           *sql.DB
	ctx          context.Context

	mu          sync.Mutex
	processed   int
	errors      int
	totalMsgs   int
	latencies   []float64
	lastFlushAt time.Time // updated by every metrics write; guarded by mu

	inFlightMu sync.Mutex
	inFlight   map[string]*inFlightEntry

	crashed atomic.Bool
}

// NewPaymentService creates a PaymentService and starts its background goroutines.
func NewPaymentService(orderQueue, paymentQueue, dlqQueue *Queue, engine *FaultEngine, db *sql.DB, ctx context.Context) *PaymentService {
	s := &PaymentService{
		orderQueue:   orderQueue,
		paymentQueue: paymentQueue,
		dlqQueue:     dlqQueue,
		engine:       engine,
		db:           db,
		ctx:          ctx,
		inFlight:     make(map[string]*inFlightEntry),
		lastFlushAt:  time.Now(),
	}
	go s.runTicker()
	go s.runMetricsWriter()
	return s
}

// Restart re-enables message processing after a cascading failure. The service
// auto-recovers when the fault is deactivated, but callers may also invoke this
// directly (e.g. from the simulator HTTP API).
func (s *PaymentService) Restart() {
	s.crashed.Store(false)
}

// ForceWriteMetrics immediately flushes accumulated counters to service_metrics
// using the actual elapsed time since the last flush. Intended for tests so
// they don't have to wait for the background timer to fire.
func (s *PaymentService) ForceWriteMetrics() {
	now := time.Now()
	s.mu.Lock()
	elapsed := now.Sub(s.lastFlushAt)
	s.mu.Unlock()
	if elapsed < time.Millisecond {
		elapsed = time.Millisecond
	}
	s.writeMetrics(elapsed)
}

// runTicker fires a processing tick every 100ms.
func (s *PaymentService) runTicker() {
	ticker := time.NewTicker(100 * time.Millisecond)
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

// processTick polls up to 5 messages from order-queue and spawns a goroutine
// per message. If the service is crashed it skips processing and checks for
// auto-recovery when the fault is deactivated.
func (s *PaymentService) processTick() {
	if s.crashed.Load() {
		// Auto-recover when the cascading-failure fault is no longer active.
		fctx := s.engine.GetFaultContext("payment")
		if !fctx.ShouldCrash {
			s.crashed.Store(false)
		}
		return
	}

	fctx := s.engine.GetFaultContext("payment")
	if fctx.ShouldCrash {
		s.crashed.Store(true)
		return
	}

	for i := 0; i < 5; i++ {
		msg, ok := s.orderQueue.TryReceive()
		if !ok {
			break
		}
		go s.processMessage(msg)
	}
}

// processMessage validates, delays, and routes a single raw order message.
// It implements retry-with-backoff before sending to the DLQ.
func (s *PaymentService) processMessage(raw string) {
	var msg orderMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		s.incrError()
		return
	}

	// Track in-flight state for this message.
	s.inFlightMu.Lock()
	entry, exists := s.inFlight[msg.MessageID]
	if !exists {
		entry = &inFlightEntry{
			attempts:       0,
			firstAttemptAt: time.Now().UnixMilli(),
		}
		s.inFlight[msg.MessageID] = entry
	}
	s.inFlightMu.Unlock()

	defer func() {
		s.inFlightMu.Lock()
		delete(s.inFlight, msg.MessageID)
		s.inFlightMu.Unlock()
	}()

	// Schema validation: poisoned messages go directly to DLQ with no retry.
	if msg.Meta.Poisoned {
		s.sendToDLQ(raw, "schema_invalid", 1, entry.firstAttemptAt)
		s.incrError()
		return
	}

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		s.inFlightMu.Lock()
		entry.attempts = attempt
		s.inFlightMu.Unlock()

		fctx := s.engine.GetFaultContext("payment")
		start := time.Now()

		var sleepDur time.Duration
		if fctx.ProcessingDelayMs > 0 {
			sleepDur = time.Duration(fctx.ProcessingDelayMs) * time.Millisecond
		} else {
			sleepDur = time.Duration(50+mrand.Intn(101)) * time.Millisecond
		}

		select {
		case <-time.After(sleepDur):
		case <-s.ctx.Done():
			return
		}

		latencyMs := float64(time.Since(start).Milliseconds())

		// Re-read fault context after the sleep so changes mid-processing are picked up.
		fctx = s.engine.GetFaultContext("payment")
		failed := fctx.ErrorProbability > 0 && mrand.Float64() < fctx.ErrorProbability

		if !failed {
			s.sendToPaymentQueue(msg, latencyMs)
			return
		}

		// Record this failed attempt.
		s.incrError()

		if attempt < maxAttempts {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-s.ctx.Done():
				return
			}
			continue
		}

		// All retries exhausted → DLQ.
		s.sendToDLQ(raw, "processing_error", attempt, entry.firstAttemptAt)
	}
}

func (s *PaymentService) sendToPaymentQueue(msg orderMessage, latencyMs float64) {
	pm := paymentMessage{
		MessageID:     newUUID(),
		OrderID:       msg.OrderID,
		CustomerID:    msg.CustomerID,
		TotalAmount:   msg.TotalAmount,
		Status:        "approved",
		TransactionID: newUUID(),
		ProcessedAt:   time.Now().UnixMilli(),
		TraceID:       msg.TraceID,
		ProcessingMs:  int64(latencyMs),
	}
	payload, err := json.Marshal(pm)
	if err != nil {
		s.incrError()
		return
	}
	s.paymentQueue.Send(string(payload))

	s.mu.Lock()
	s.processed++
	s.totalMsgs++
	s.latencies = append(s.latencies, latencyMs)
	s.mu.Unlock()
}

func (s *PaymentService) sendToDLQ(raw, reason string, attempts int, firstAttemptAt int64) {
	dm := dlqMessage{
		OriginalMessage: json.RawMessage(raw),
		FailureReason:   reason,
		AttemptCount:    attempts,
		FirstAttemptAt:  firstAttemptAt,
		LastAttemptAt:   time.Now().UnixMilli(),
	}
	payload, _ := json.Marshal(dm)
	s.dlqQueue.Send(string(payload))
}

func (s *PaymentService) incrError() {
	s.mu.Lock()
	s.errors++
	s.totalMsgs++
	s.mu.Unlock()
}

// runMetricsWriter writes a service_metrics row every 2–3s (jittered).
func (s *PaymentService) runMetricsWriter() {
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

func (s *PaymentService) writeMetrics(elapsed time.Duration) {
	s.mu.Lock()
	processed := s.processed
	errors := s.errors
	totalMsgs := s.totalMsgs
	latencies := s.latencies
	s.processed = 0
	s.errors = 0
	s.totalMsgs = 0
	s.latencies = nil
	s.lastFlushAt = time.Now()
	s.mu.Unlock()

	elapsedSec := elapsed.Seconds()
	throughput := float64(processed) / elapsedSec

	var errorRate float64
	if totalMsgs > 0 {
		errorRate = float64(errors) / float64(totalMsgs)
	}

	p50, p99 := percentiles(latencies)
	activeFaults := activeFaultNames(s.engine)

	ts := time.Now().UnixMilli()
	_, _ = s.db.Exec(
		`INSERT INTO service_metrics (ts, service, throughput, error_rate, p50_latency, p99_latency, active_faults)
		 VALUES (?, 'payment', ?, ?, ?, ?, ?)`,
		ts, throughput, errorRate, p50, p99, activeFaults,
	)
}
