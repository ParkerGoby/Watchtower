package simulator

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	mrand "math/rand"
	"sync"
	"time"
)

// notificationDLQMessage is the dead-letter envelope for messages that exhaust
// notification retries. Unlike the payment DLQ, this queue has no consumer —
// messages accumulate indefinitely, modelling the silent failure pattern.
type notificationDLQMessage struct {
	OriginalMessage  json.RawMessage `json:"originalMessage"`
	FailureReason    string          `json:"failureReason"`
	AttemptCount     int             `json:"attemptCount"`
	FirstAttemptAt   int64           `json:"firstAttemptAt"`
	LastAttemptAt    int64           `json:"lastAttemptAt"`
	NotificationType string          `json:"notificationType"` // "confirmation" | "declined"
}

// NotificationService consumes payment messages via a fan-out subscription from
// payment-queue and simulates sending order notifications. It runs in parallel
// with FulfillmentService — each service gets its own independent copy of every
// payment message (fan-out pattern).
//
// Its primary observable failure mode is silent DLQ accumulation: at a 15%
// error rate with only 2 retries, the notification-dlq fills quietly while the
// aggregate error_rate stays below the anomaly threshold.
type NotificationService struct {
	paymentSub *Queue
	notifDLQ   *Queue
	engine     *FaultEngine
	db         *sql.DB
	ctx        context.Context

	mu          sync.Mutex
	processed   int
	errors      int
	totalMsgs   int
	latencies   []float64
	lastFlushAt time.Time
}

// NewNotificationService creates a NotificationService and starts its background
// goroutines. paymentSub must be a fan-out subscription from payment-queue
// (created via Queue.Subscribe).
func NewNotificationService(paymentSub *Queue, notifDLQ *Queue, engine *FaultEngine, db *sql.DB, ctx context.Context) *NotificationService {
	s := &NotificationService{
		paymentSub:  paymentSub,
		notifDLQ:    notifDLQ,
		engine:      engine,
		db:          db,
		ctx:         ctx,
		lastFlushAt: time.Now(),
	}
	go s.runTicker()
	go s.runMetricsWriter()
	return s
}

// ForceWriteMetrics immediately flushes accumulated counters to service_metrics.
// Intended for tests so they don't have to wait for the background timer to fire.
func (s *NotificationService) ForceWriteMetrics() {
	now := time.Now()
	s.mu.Lock()
	elapsed := now.Sub(s.lastFlushAt)
	s.mu.Unlock()
	if elapsed < time.Millisecond {
		elapsed = time.Millisecond
	}
	s.writeMetrics(elapsed)
}

// runTicker fires a processing tick every 150ms.
func (s *NotificationService) runTicker() {
	ticker := time.NewTicker(150 * time.Millisecond)
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

// processTick polls up to 3 messages from the subscription queue and spawns a
// goroutine per message. When the queue is empty this is a no-op — starvation
// requires no special handling.
func (s *NotificationService) processTick() {
	for i := 0; i < 3; i++ {
		msg, ok := s.paymentSub.TryReceive()
		if !ok {
			break
		}
		go s.processMessage(msg)
	}
}

// processMessage simulates sending a notification for one payment message.
// Approved messages produce a "confirmation" notification; declined messages
// produce a "declined" notification. Failed attempts are retried once (2
// total attempts); exhausted retries are sent to notification-dlq.
func (s *NotificationService) processMessage(raw string) {
	var msg paymentMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		s.mu.Lock()
		s.errors++
		s.totalMsgs++
		s.mu.Unlock()
		return
	}

	notifType := "confirmation"
	if msg.Status == "declined" {
		notifType = "declined"
	}

	firstAttemptAt := time.Now().UnixMilli()
	const maxAttempts = 2

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		start := time.Now()

		sleepDur := time.Duration(10+mrand.Intn(21)) * time.Millisecond
		select {
		case <-time.After(sleepDur):
		case <-s.ctx.Done():
			return
		}

		latencyMs := float64(time.Since(start).Milliseconds())

		fctx := s.engine.GetFaultContext("notification")
		failed := fctx.ErrorProbability > 0 && mrand.Float64() < fctx.ErrorProbability

		if !failed {
			s.mu.Lock()
			s.processed++
			s.totalMsgs++
			s.latencies = append(s.latencies, latencyMs)
			s.mu.Unlock()
			return
		}

		s.mu.Lock()
		s.errors++
		s.totalMsgs++
		s.mu.Unlock()

		if attempt < maxAttempts {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-s.ctx.Done():
				return
			}
			continue
		}

		// All retries exhausted → DLQ.
		s.sendToDLQ(raw, "processing_error", attempt, firstAttemptAt, notifType)
		log.Printf("notification: DLQ message %s after %d attempts", msg.MessageID, maxAttempts)
	}
}

func (s *NotificationService) sendToDLQ(raw, reason string, attempts int, firstAttemptAt int64, notifType string) {
	dm := notificationDLQMessage{
		OriginalMessage:  json.RawMessage(raw),
		FailureReason:    reason,
		AttemptCount:     attempts,
		FirstAttemptAt:   firstAttemptAt,
		LastAttemptAt:    time.Now().UnixMilli(),
		NotificationType: notifType,
	}
	payload, _ := json.Marshal(dm)
	s.notifDLQ.Send(string(payload))
}

// runMetricsWriter writes a service_metrics row every 2–3s (jittered).
func (s *NotificationService) runMetricsWriter() {
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

func (s *NotificationService) writeMetrics(elapsed time.Duration) {
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
		 VALUES (?, 'notification', ?, ?, ?, ?, ?)`,
		ts, throughput, errorRate, p50, p99, activeFaults,
	)
}
