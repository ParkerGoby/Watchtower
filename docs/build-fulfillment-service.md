# Build Plan: Fulfillment Service

## Role in the System

The Fulfillment Service is a pure consumer. It reads from `payment-queue`, processes each approved payment into a fulfillment record (warehouse pick, ship, confirm), and produces no downstream queue output. It is the end of the order pipeline.

Fulfillment's primary failure mode is **starvation**: when the Payment Service stops producing to `payment-queue` — whether from a crash, timeout, or DLQ-filling poison pill — Fulfillment silently goes idle. Its `throughput` drops to zero. It emits no errors, raises no alarms on its own. The monitoring challenge is recognizing that "this service is unusually quiet" is just as bad a signal as "this service is erroring."

---

## Happy Path Behavior

1. Polls `payment-queue` for messages via `queue.receive('payment-queue', { maxMessages: 3 })`
2. For each `PaymentMessage`:
   - If `status === 'declined'`: logs the decline, deletes from queue, records as a non-error outcome (declines are expected business behavior)
   - If `status === 'approved'`: simulates fulfillment steps with a ~30–80ms processing delay
3. Deletes the message from `payment-queue` on successful processing
4. Writes `service_metrics` every 2–3 seconds

Fulfillment does not have a DLQ. If it fails to process a message (rare in the happy path), it re-queues by simply not deleting the message from `payment-queue` (fake SQS visibility timeout behavior). After 3 re-attempts it logs an error and discards the message — in a real system, `payment-queue` would be configured with its own DLQ, but for this simulation Fulfillment's error rate is kept very low by design.

---

## Queue Interface

### Consumes

**Queue:** `payment-queue`

Reads up to 3 messages per tick. Processes synchronously within the tick. Deletes on success.

### Produces

None. Fulfillment has no downstream queue.

---

## Metrics Emitted

| Column | How it's calculated |
|---|---|
| `service` | `'fulfillment'` |
| `ts` | `Date.now()` |
| `throughput` | Orders fulfilled since last write ÷ elapsed seconds |
| `error_rate` | Failed fulfillment attempts ÷ total attempts (normally near zero) |
| `p50_latency` | Median time from message receipt to fulfillment completion, in ms |
| `p99_latency` | 99th-percentile of same |
| `active_faults` | Active fault names from fault engine |

**Key metric for starvation detection:** `throughput`. A healthy Fulfillment Service maintains ~3–4 fulfillments/s under normal load. A throughput value below 0.5 for more than two consecutive metric windows is an anomaly signal.

---

## Starvation Behavior

Starvation occurs when `payment-queue` depth approaches zero and stays there — not because Fulfillment is fast, but because Payment is not producing.

The distinction matters for root cause inference:

| Queue depth | Fulfillment throughput | Interpretation |
|---|---|---|
| High | Low | Fulfillment is slow or erroring (Fulfillment is the problem) |
| Low | Low | Queue is empty — upstream is the problem |
| Low | Normal | Healthy — Fulfillment is keeping up |

The monitor detects starvation by joining `queue_metrics` for `payment-queue` with `service_metrics` for `fulfillment` within the same time window. If both depth and throughput are anomalously low simultaneously, the correlation engine classifies this as a starvation event rather than a Fulfillment error.

**Implementation:** The Fulfillment Service does not need to do anything special to simulate starvation. It simply polls the queue on its normal tick schedule. When the queue is empty, `queue.receive()` returns an empty array, and the tick does nothing. `throughput` naturally falls to 0. This emergent behavior is the correct simulation.

---

## Fault Behavior

### Poison Pill (indirect)

- Fulfillment is not the source of the fault
- Payment Service errors cause `payment-queue` to stop receiving new messages
- `payment-queue` depth drops to 0; Fulfillment throughput drops to 0
- Fulfillment `error_rate` stays 0 (it has nothing to process, so nothing to fail)
- This is the starvation pattern described above

### Downstream Timeout (indirect)

- Same starvation pattern as poison pill, but the lead indicator is `payment-queue` depth remaining low (Payment is hanging on each message, nothing completes to the queue)
- Fulfillment throughput gradually drops as the queue drains
- The drop is gradual rather than sudden because the queue had existing depth when the timeout started

### Traffic Spike (indirect)

- Payment processes a burst of messages — `payment-queue` depth spikes temporarily
- Fulfillment processes them at its normal rate; it cannot burst
- `payment-queue` depth builds up in front of Fulfillment
- Fulfillment `throughput` rises modestly (it polls up to 3 per tick, but this is already near its max)
- `p99_latency` may rise slightly if the batch size causes tick overruns
- **This is not a starvation event** — queue depth is high, throughput is normal. The anomaly is the queue depth, not the service.

### Cascading Failure (indirect)

- Payment Service crashes entirely; no new messages reach `payment-queue`
- Fulfillment drains whatever was already in the queue, then goes idle
- Transition is fast: under normal load, `payment-queue` has only a few messages in it at any time
- Fulfillment throughput → 0 within ~5–10 seconds of the crash

### Random Intermittent Errors

- `faultContext.errorProbability = 0.15` applies to Fulfillment
- 15% of processing attempts "fail" — simulated by throwing an error in the processing step
- Failed messages are retried (visibility timeout re-delivery); most succeed on retry
- Net effect: `error_rate` rises modestly (~0.05 after retry), `throughput` drops slightly (retry latency)
- In isolation, this does not trigger an incident — it's below the anomaly threshold
- When combined with similar signals from other services, the correlation engine groups them as the intermittent error fault

---

## Implementation Notes

- Fulfillment runs a goroutine driven by a `time.NewTicker` at ~150ms ticks — it is not the throughput-limiting step
- Processing delay is sampled from a uniform distribution 30–80ms per message via `time.Sleep()`; this produces realistic-looking latency histograms
- Declined payments count toward throughput (they were processed) but not as errors
- The service tracks its own `idleTicks` counter: consecutive ticks with zero messages available. This is used by the monitor to distinguish "empty queue" from "service not running."

---

## Acceptance Criteria

| Scenario | Observable outcome |
|---|---|
| Happy path 60s | `throughput` between 2–5 msg/s; `error_rate` ≤ 0.02; `payment-queue` depth ≤ 5 |
| Payment Service crashed | Fulfillment `throughput` → 0 within 15s; `error_rate` remains 0; `payment-queue` depth → 0 (drained) |
| Payment DLQ filling (poison pill) | Fulfillment `throughput` → 0 within 20s; `error_rate` stays 0 |
| Traffic spike | Fulfillment throughput rises slightly but is bounded by poll batch size; `payment-queue` depth rises; no errors |
| Starvation vs. Fulfillment error correctly distinguished | Monitor classifies low-throughput + low-queue-depth as starvation (upstream cause); low-throughput + high-queue-depth as Fulfillment error |
