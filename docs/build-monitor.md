# Build Plan: Monitor

## Overview

The monitor is a Go process that runs independently of the simulator. It reads from the shared SQLite database, detects anomalies, runs the correlation engine, writes incident records, and exposes an HTTP API for the dashboard.

The monitor has no side effects on the simulation — it is read-only against the `service_metrics` and `queue_metrics` tables. It only writes to `incidents`.

---

## Process Structure

```
internal/monitor/
  poller.go         -- main polling ticker (2s interval via time.Ticker)
  anomaly.go        -- baseline windowing and threshold logic
  correlator.go     -- correlation engine
  incidents.go      -- incident creation, update, resolution
  api.go            -- net/http router and handler registration
  sse.go            -- SSE broadcaster (tracks connected clients)

internal/db/
  db.go             -- SQLite connection (modernc.org/sqlite), query helpers
  schema.go         -- CREATE TABLE statements and migration

cmd/monitor/
  main.go           -- entry point; wires up DB, starts poller and HTTP server
```

---

## Polling Loop

The polling loop is the heartbeat of the monitor. It runs every **2 seconds** via `time.NewTicker(2 * time.Second)` in its own goroutine.

### Each tick:

1. **Read latest metrics:** Query `service_metrics` and `queue_metrics` for all rows written in the last 30 seconds. This is a bounded read — the window is large enough to catch any new data but small enough to stay fast.

2. **Update baselines:** For each service and queue, update the rolling baseline with the new data points. Baselines cover the last 60 seconds (configurable).

3. **Run anomaly detection:** Compare the most recent metric values against each baseline. Emit `AnomalySignal` objects for any metric that exceeds its threshold.

4. **Run correlation engine:** Pass the full set of active `AnomalySignal` objects to the correlator. Receive zero or more `CorrelatedEvent` objects.

5. **Reconcile incidents:** For each `CorrelatedEvent`, check whether an active incident already exists (by matching `root_service` and `fault_type`). If not, create a new `incidents` row. If yes, update the `timeline` with any new events.

6. **Resolve incidents:** For each active incident, check whether all signals in its `affected` set have returned to baseline. If yes, mark resolved.

7. **Broadcast SSE:** Send the current state snapshot to all connected SSE clients.

---

## Anomaly Detection

### Baseline Model

For each `(service, metric)` and `(queue, metric)` pair, the monitor maintains a sliding window of values from the last **60 seconds** of polling cycles. From this window it computes:

- `mean`: arithmetic mean of all values in the window
- `stddev`: population standard deviation
- `count`: number of samples in the window

The baseline is only considered valid when `count >= 10` (about 20 seconds of data). Before the baseline is established, no anomalies are emitted. This prevents false positives at startup.

### Thresholds

| Metric | Threshold | Minimum floor |
|---|---|---|
| `error_rate` | `mean + 3σ` | 0.10 (never alert below 10% error rate) |
| `p99_latency` | `mean + 3σ` | 500ms |
| `throughput` (low) | `mean - 2σ` | Must be below 0.5 msg/s |
| `queue_depth` (high) | `mean + 2σ` | 20 messages |
| `dlq_depth` (any) | > 0 | — (always an anomaly) |

### `AnomalySignal` Shape

```go
type AnomalySignal struct {
    TS        int64   `json:"ts"`        // Unix ms of the metric row that triggered this
    Service   string  `json:"service"`   // for service metrics (empty if queue signal)
    Queue     string  `json:"queue"`     // for queue metrics (empty if service signal)
    Metric    string  `json:"metric"`    // "error_rate" | "p99_latency" | "throughput" | "queue_depth" | "dlq_depth"
    Value     float64 `json:"value"`     // the anomalous value
    Baseline  float64 `json:"baseline"`  // the mean at time of detection
    Deviation float64 `json:"deviation"` // (value - baseline) / stddev
    Direction string  `json:"direction"` // "high" | "low"
}
```

Signals are deduplicated within a single polling cycle: if the same `(service, metric)` pair generates two signals from two metric rows in the same cycle, only the more extreme one is kept.

---

## Correlation Engine

The correlator takes the full set of currently active `AnomalySignal` objects (accumulated across polling cycles, not just the latest) and returns `CorrelatedEvent` objects representing grouped incidents.

### Correlation Window

Active signals are those with `ts` within the last **30 seconds** of the current time. Signals older than 30 seconds are pruned from the active set unless they belong to an already-open incident.

### Grouping Algorithm

1. Sort active signals by `ts` ascending.
2. Open a new group with the first unassigned signal.
3. For each subsequent signal, add it to the current group if `signal.ts - group.startTs <= 10000` (10-second window). Otherwise, close the current group and open a new one.
4. Groups with only one signal are not promoted to incidents unless the single signal is a DLQ signal (any DLQ activity is worth surfacing individually).

### Root Cause Nomination

Given a group of signals, the correlator applies the topology graph to nominate a root cause:

**Topology order (upstream → downstream):**
```
api-gateway (depth 0)
  └─ order (depth 1)
       └─ payment (depth 2)
            ├─ fulfillment (depth 3)
            └─ notification (depth 3)
```

Rules:
1. The signal with the **smallest topology depth** is the primary candidate.
2. If two signals share the same depth, the one with the **earlier timestamp** wins.
3. If a `fault_events` row exists with `action = 'start'` within 5 seconds before `group.startTs`, and its `origin` maps to a service in the group, that service is used as root cause regardless of topology — direct evidence beats inference.
4. If the root candidate is a queue (not a service), the root cause is attributed to the service that **writes to** that queue.

**Special cases:**

- Starvation (low throughput + low queue depth): root cause is attributed to the upstream service, not the starved service
- DLQ-only signal (no correlated service signals): root cause is attributed to the service that writes to that DLQ, with note "silent failure — no correlated upstream anomaly"

### Blast Radius Inference

After nominating the root cause, the correlator infers the blast radius:

```
affected = [root_service] ∪ { all services downstream of root in topology that have active signals }
```

If a downstream service has no active signal but its upstream is failing and sufficient time has passed, the correlator adds it to `affected` with a note `"anticipated starvation — no signal yet"`.

### `CorrelatedEvent` Shape

```go
type CorrelatedEvent struct {
    StartTS     int64          `json:"startTs"`
    RootService string         `json:"rootService"`
    RootCause   string         `json:"rootCause"`   // human-readable
    FaultType   string         `json:"faultType"`   // empty if not matched from fault_events
    Severity    string         `json:"severity"`    // "warning" | "critical"
    Affected    []string       `json:"affected"`
    Signals     []AnomalySignal `json:"signals"`
    Timeline    []TimelineEvent `json:"timeline"`
}
```

**Severity rules:**
- `critical` if: any service has `shouldCrash` in its active faults, or any DLQ depth > 50, or `payment` throughput = 0 for > 10s
- `warning` otherwise

---

## Incident Data Model

Full schema is in [`architecture.md`](architecture.md#sqlite-schema), but the key behavioral notes:

- An incident is created the first time a `CorrelatedEvent` has no matching open incident
- Multiple polling cycles can contribute new `TimelineEvent` rows to the same incident (the `timeline` JSON is updated in place)
- An incident is resolved when **all** services in `affected` have returned to within 1.5σ of their baselines for **two consecutive** polling cycles
- Resolved incidents are never deleted — they are the historical record the dashboard's incident log displays
- The `fault_type` column is set when a matching `fault_events` row is found; it may be `NULL` for incidents where no explicit fault was injected (e.g., emergent behavior from multiple intermittent error injections)

---

## API Routes

The monitor runs a `net/http` server on port **3001**.

### `GET /api/state`

Returns a full snapshot of current system state. Called by the dashboard on initial load and on each SSE reconnect.

**Response:**

```json
{
  "ts": 1710000000000,
  "services": [
    {
      "name": "payment",
      "throughput": 4.2,
      "errorRate": 0.01,
      "p50Latency": 87,
      "p99Latency": 143,
      "status": "healthy",
      "activeFaults": []
    }
  ],
  "queues": [
    {
      "name": "payment-dlq",
      "depth": 0,
      "enqueueRate": 0,
      "dequeueRate": 0,
      "isDlq": true
    }
  ],
  "incidents": [ /* active incidents only */ ],
  "baselineReady": true
}
```

### `GET /api/incidents`

Returns all incidents (active and resolved), ordered by `started_at` descending. Supports `?limit=20&offset=0` pagination.

### `GET /api/incidents/:id`

Returns a single incident with its full `timeline` array.

### `GET /api/metrics/history`

Returns raw metric history for charting. Query params:

| Param | Default | Description |
|---|---|---|
| `service` | (all) | Filter to one service or queue name |
| `window` | `300` | Seconds of history to return |
| `metric` | `throughput` | Which metric column |

### `GET /api/events/stream`

SSE endpoint. The monitor broadcasts a `state` event every 2 seconds containing the same payload as `GET /api/state`. The dashboard subscribes to this on mount and updates its React state on each event.

**SSE event format:**
```
event: state
data: { ...same as /api/state }
```

**SSE event: `incident`** — emitted immediately (not on the 2s cycle) when a new incident is created or an existing one changes severity or resolves.

---

## Error Handling

- If the SQLite database is not available at startup, the monitor retries every 5 seconds with a logged warning
- If a polling cycle takes longer than 1800ms, the next cycle is skipped and a warning is logged (prevents overlap)
- If the correlator throws an unexpected error, it is caught, logged, and the polling cycle continues — anomaly signals are not lost
- SSE clients that disconnect are cleaned up from the broadcaster's client list

---

## Acceptance Criteria

| Scenario | Observable outcome |
|---|---|
| Monitor starts with empty DB | No anomalies emitted; `baselineReady: false` until 10+ samples exist |
| Happy path 30s | `GET /api/state` returns all services healthy; no active incidents |
| Poison pill injected | Within 3 polling cycles (6s): incident created with `root_service: 'payment'` or `'order'` and `fault_type: 'poison-pill'`; Payment and Fulfillment in `affected` |
| Cascading failure injected | `critical` incident within 2 cycles; `root_service: 'payment'`; Fulfillment in `affected` |
| DLQ-only signal (notification) | `warning` incident created; `fault_type` may be null |
| Fault deactivated | Incident `resolved_at` set within 4 cycles of signal return to baseline |
| SSE stream | `GET /api/events/stream` delivers `state` events at ~2s intervals; `incident` event emitted within 500ms of incident creation |
