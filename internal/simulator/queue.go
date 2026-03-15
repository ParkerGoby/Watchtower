package simulator

import (
	"context"
	"database/sql"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const queueBufferSize = 1000

// Queue is an in-memory message queue that periodically writes metrics to
// SQLite. It is safe for concurrent use by multiple producers and consumers.
type Queue struct {
	name     string
	isDLQ    bool
	ch       chan string
	db       *sql.DB
	depth    atomic.Int64
	enqueued atomic.Int64
	dequeued atomic.Int64
	ctx      context.Context

	mu          sync.Mutex
	lastFlushAt time.Time
}

// NewQueue creates a queue with the given name and starts the background
// metrics writer. isDLQ=true sets dlq=1 in every metrics row.
func NewQueue(name string, isDLQ bool, conn *sql.DB, ctx context.Context) *Queue {
	q := &Queue{
		name:        name,
		isDLQ:       isDLQ,
		ch:          make(chan string, queueBufferSize),
		db:          conn,
		ctx:         ctx,
		lastFlushAt: time.Now(),
	}
	go q.runMetricsWriter()
	return q
}

// Send enqueues a message. It returns immediately if the buffer is not full.
// If the buffer is full, Send blocks until space is available or the context
// is cancelled.
func (q *Queue) Send(msg string) {
	select {
	case q.ch <- msg:
		q.depth.Add(1)
		q.enqueued.Add(1)
	case <-q.ctx.Done():
	}
}

// Receive dequeues the next message. Returns (msg, true) if a message is
// available, or ("", false) if the context is cancelled and the queue is empty.
func (q *Queue) Receive() (string, bool) {
	select {
	case msg := <-q.ch:
		q.depth.Add(-1)
		q.dequeued.Add(1)
		return msg, true
	case <-q.ctx.Done():
		// Drain one message if available before giving up.
		select {
		case msg := <-q.ch:
			q.depth.Add(-1)
			q.dequeued.Add(1)
			return msg, true
		default:
			return "", false
		}
	}
}

// TryReceive dequeues a message without blocking.
// Returns ("", false) immediately if no message is available.
func (q *Queue) TryReceive() (string, bool) {
	select {
	case msg := <-q.ch:
		q.depth.Add(-1)
		q.dequeued.Add(1)
		return msg, true
	default:
		return "", false
	}
}

// Depth returns the current number of messages in the queue.
func (q *Queue) Depth() int {
	return int(q.depth.Load())
}

// ForceWriteMetrics immediately flushes accumulated counters to queue_metrics
// using the actual elapsed time since the last flush. Intended for tests so
// they don't have to wait for the background timer to fire.
func (q *Queue) ForceWriteMetrics() {
	now := time.Now()
	q.mu.Lock()
	elapsed := now.Sub(q.lastFlushAt)
	q.mu.Unlock()
	if elapsed < time.Millisecond {
		elapsed = time.Millisecond
	}
	q.writeMetrics(elapsed)
}

// runMetricsWriter writes a queue_metrics row every 2–3 seconds (jittered)
// until the context is cancelled.
func (q *Queue) runMetricsWriter() {
	for {
		// Jitter between 2000ms and 3000ms.
		delay := time.Duration(2000+rand.Intn(1000)) * time.Millisecond
		select {
		case <-time.After(delay):
			q.writeMetrics(delay)
		case <-q.ctx.Done():
			return
		}
	}
}

func (q *Queue) writeMetrics(elapsed time.Duration) {
	q.mu.Lock()
	q.lastFlushAt = time.Now()
	q.mu.Unlock()

	elapsedSec := elapsed.Seconds()

	enqueued := q.enqueued.Swap(0)
	dequeued := q.dequeued.Swap(0)

	enqRate := float64(enqueued) / elapsedSec
	deqRate := float64(dequeued) / elapsedSec
	depth := q.depth.Load()

	dlq := 0
	if q.isDLQ {
		dlq = 1
	}

	ts := time.Now().UnixMilli()
	_, _ = q.db.Exec(
		`INSERT INTO queue_metrics (ts, queue, depth, enqueue_rate, dequeue_rate, dlq) VALUES (?, ?, ?, ?, ?, ?)`,
		ts, q.name, depth, enqRate, deqRate, dlq,
	)
}
