# Build Plan: Notification Service

## Role in the System

The Notification Service runs in parallel with Fulfillment. It also consumes from `payment-queue`, but it is a secondary consumer — it reads copies of `PaymentMessage` records to send order confirmation and payment notifications to the customer (email/SMS in a real system; no-op in this simulation).

The Notification Service is architecturally interesting because it is the lowest-priority consumer in the topology. In a real system, notifications are often fire-and-forget: losing a notification is unfortunate but not business-critical. This makes it an ideal place to demonstrate **silent failure** — where a service's DLQ fills quietly because nobody is watching it.

The `notification-dlq` is the least observed metric in the happy path. This is by design: the demo shows that silent DLQ accumulation is a real production risk, and the correlation engine should surface it even when no other service is alerting.

---

## Topology Note

The Notification Service consumes from `payment-queue` alongside Fulfillment. This is modeled as a fan-out pattern: both services see the same messages. In the simulation, this is implemented by having the queue deliver each message to both consumers (rather than competing consumers consuming different messages). This is consistent with how SNS → SQS fan-out works in real AWS.

```
payment-queue
    ├──▶ Fulfillment Service
    └──▶ Notification Service ──[notification-dlq]
```

---

## Happy Path Behavior

1. Polls `payment-queue` (its own logical subscription) for messages
2. For each `PaymentMessage`:
   - If `status === 'approved'`: simulates sending a confirmation notification (~10–30ms delay)
   - If `status === 'declined'`: simulates sending a declined payment notification (~10–30ms delay)
3. Deletes the message from its subscription on success
4. On processing failure: retries up to 2 times, then sends to `notification-dlq`
5. Writes `service_metrics` every 2–3 seconds

Notification has lower retry tolerance than Payment (2 attempts vs 3) because notification failures are lower priority — the system would rather DLQ a notification than spend time retrying it.

---

## Queue Interface

### Consumes

**Queue:** `payment-queue` (logical fan-out subscription — does not compete with Fulfillment)

### Produces

**Queue:** `notification-dlq`

**Message shape:**

```
NotificationDLQMessage {
  originalMessage: PaymentMessage
  failureReason:   string        // e.g. 'processing_error' | 'schema_invalid'
  attemptCount:    number        // max 2
  firstAttemptAt:  number        // Unix ms
  lastAttemptAt:   number        // Unix ms
  notificationType: 'confirmation' | 'declined'
}
```

---

## Metrics Emitted

| Column | How it's calculated |
|---|---|
| `service` | `'notification'` |
| `ts` | `Date.now()` |
| `throughput` | Notifications sent since last write ÷ elapsed seconds |
| `error_rate` | Failed notification attempts ÷ total attempts |
| `p50_latency` | Median time from message receipt to notification "sent" |
| `p99_latency` | 99th-percentile of same |
| `active_faults` | Active fault names |

The `notification-dlq` queue writes its own `queue_metrics` row with `dlq = 1`. DLQ depth is the primary alert signal for this service.

---

## Silent Failure Patterns

Silent failure occurs when the Notification Service fails consistently but nothing in the system is visibly broken. Examples:

**Pattern 1: DLQ fills without error rate spike**

If the Notification Service has a bug that causes it to fail on a specific message shape (e.g., missing a field) but succeed on all others, its aggregate `error_rate` may stay low (say, 3–5%) while the DLQ accumulates steadily. Without explicit DLQ monitoring, this goes unnoticed until the DLQ is full and the service is silently dropping all messages of that shape.

This is simulated during the **random intermittent errors** fault: some messages fail, go to DLQ, but the aggregate error rate stays below the alert threshold. The DLQ depth is the only clear signal.

**Pattern 2: Fan-out consumer lag**

If Fulfillment is consuming from `payment-queue` normally, the queue depth stays low. But if Notification is falling behind on its logical subscription, **its own** effective queue depth grows. In a real SQS fan-out setup (SNS delivering to separate SQS queues), this would be visible as subscription-queue depth. In this simulation, Notification tracks its own backlog in memory and reports it as a separate metric.

**Pattern 3: Post-crash silence**

After a cascading failure fault deactivates the Payment Service, `payment-queue` stops receiving messages. Both Fulfillment and Notification go idle. Once the fault resolves and Payment restarts, Fulfillment recovers visibly (throughput rises). But if there was a backlog in Notification's DLQ from before the crash, those messages are never re-processed — and if no one checks the DLQ, the notification for those orders is simply lost.

---

## Fault Behavior

### Poison Pill (indirect)

- `payment-queue` stops receiving approved payments (Payment is DLQ-ing everything)
- Notification sees no new messages — throughput → 0
- `notification-dlq` depth stays 0 (no messages to fail on)
- This is a starvation event, not a silent failure — the DLQ is not the signal here

### Downstream Timeout (indirect)

- Same starvation pattern as Fulfillment
- Notification throughput drops as `payment-queue` drains
- No DLQ activity

### Traffic Spike (indirect)

- `payment-queue` has a burst of messages
- Notification processes at its normal rate (~5/s); burst causes a temporary backlog in its subscription
- If the burst is large enough, some messages may time out and be retried, leading to a small DLQ spike
- `notification-dlq` shows a brief depth increase then returns to 0 as the burst is absorbed

### Cascading Failure (indirect)

- Payment Service crashes; Notification drains its subscription and goes idle
- Any messages in-flight during the crash may fail and go to `notification-dlq` (simulated race condition)
- `notification-dlq` shows a small depth spike at crash time, then stabilizes

### Random Intermittent Errors

- `faultContext.errorProbability = 0.15`
- 15% of notification attempts fail
- With only 2 retries, ~2% of messages exhaust retries and go to `notification-dlq`
- `notification-dlq` depth climbs slowly but steadily during this fault
- `error_rate` shows moderate elevation (~0.08 after retry success)
- **This is the primary demonstration of silent failure:** the error rate is not alarming, but the DLQ is accumulating. The monitor surfaces this as a `warning`-level incident specifically because of DLQ activity.

---

## Implementation Notes

- The fan-out simulation is implemented by having the queue maintain a separate "read pointer" per consumer. Each consumer's pointer advances independently when messages are deleted. This mimics the SNS → separate SQS queue pattern without requiring actual message duplication.
- Notification's processing is entirely no-op (no real HTTP call, no real email). The delay is simulated with `time.Sleep()`.
- The `notification-dlq` has no consumer. Messages accumulate indefinitely until the simulator is reset. This is intentional — it models real production DLQs that nobody drains.
- The Notification Service is intentionally lower-fidelity than Payment and Fulfillment. It doesn't need deep retry logic because its role in the demo is to show DLQ patterns, not processing patterns.

---

## Acceptance Criteria

| Scenario | Observable outcome |
|---|---|
| Happy path 60s | `throughput` between 2–5 msg/s; `error_rate` ≤ 0.02; `notification-dlq` depth = 0 |
| Random intermittent errors (60s) | `notification-dlq` depth > 0 and growing; `error_rate` between 0.05–0.15; no full incident triggered by this alone |
| Cascading failure | Notification throughput → 0 within 15s; possible small DLQ spike at crash boundary |
| DLQ surfaced in dashboard | `notification-dlq` depth shown as red badge on Notification Service card regardless of `error_rate` value |
| Silent failure detected | Monitor creates `warning`-level incident when `notification-dlq` depth > 0 for 2 consecutive polling cycles, even when `error_rate` < anomaly threshold |
