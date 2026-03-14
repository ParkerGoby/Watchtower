# Architecture

## Overview

Watchtower is split into three components that run as separate processes but share a single SQLite database file:

| Component | Language | Role |
|---|---|---|
| `cmd/simulator` | Go | Goroutine-based workers that simulate the microservice topology and write metrics to SQLite |
| `cmd/monitor` | Go | Polling ticker, anomaly detection, and correlation engine — reads metrics from SQLite, writes incidents, serves HTTP API |
| `dashboard/` | TypeScript / Next.js | Frontend — reads from the monitor's HTTP API and renders the live UI |

The simulator and monitor are intentionally decoupled. The simulator's only job is to produce realistic metric data. The monitor's only job is to interpret it. This separation means the correlation engine can be tested independently against recorded metric traces without needing the simulator running.

---

## Component Diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│  Simulator Process                                                   │
│                                                                      │
│  ┌─────────────┐    ┌───────────────┐    ┌─────────────────────┐   │
│  │ API Gateway │───▶│ Order Service │───▶│  [order-queue]      │   │
│  │  (fake HTTP)│    │               │    │  (in-memory)        │   │
│  └─────────────┘    └───────────────┘    └──────────┬──────────┘   │
│                                                      │              │
│                             ┌────────────────────────▼──────────┐  │
│                             │  Payment Service                   │  │
│                             │                                    │  │
│                             │  ┌──────────────┐  ┌───────────┐  │  │
│                             │  │ [payment-dlq]│  │[pay-queue]│  │  │
│                             │  └──────────────┘  └─────┬─────┘  │  │
│                             └────────────────────────── │ ───────┘  │
│                                              ┌──────────▼──────┐    │
│                             ┌────────────────┤Fulfillment Svc  │    │
│                             │                └─────────────────┘    │
│                             │                                        │
│                     ┌───────▼──────────┐                            │
│                     │Notification Svc  │                            │
│                     │  [notif-dlq]     │                            │
│                     └──────────────────┘                            │
│                                                                      │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │  Fault Engine — injects faults into services/queues on cmd  │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                            │ writes every 2–3s                       │
└────────────────────────────│────────────────────────────────────────┘
                             ▼
                    ┌─────────────────┐
                    │   SQLite DB     │
                    │  watchtower.db  │
                    └────────┬────────┘
                             │ reads
               ┌─────────────▼─────────────┐
               │   Monitor Process          │
               │                            │
               │  Polling loop (2s)         │
               │  Anomaly detector          │
               │  Correlation engine        │
               │  HTTP API (net/http)       │
               └─────────────┬─────────────┘
                             │ HTTP / SSE
               ┌─────────────▼─────────────┐
               │   Dashboard (Next.js)      │
               │   localhost:3000           │
               └────────────────────────────┘
```

---

## Simulator ↔ Monitor Communication

Communication happens exclusively through SQLite. The simulator writes; the monitor reads.

This was a deliberate choice. An event bus or shared memory approach would couple the two processes and complicate testing. With SQLite:

- The monitor can replay any historical window
- Tests can seed the database with pre-recorded traces and assert correlation output without running the simulator
- The database file can be committed to the repo as a fixture for demo purposes
- There is no message serialization format to maintain

The only constraint is that both processes must agree on the database schema, which is defined once in `internal/db/schema.go` and applied by the simulator at startup via migration.

---

## SQLite Schema

### `service_metrics`

Written by each service worker every 2–3 seconds.

```sql
CREATE TABLE service_metrics (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  ts            INTEGER NOT NULL,           -- Unix ms timestamp
  service       TEXT    NOT NULL,           -- 'order' | 'payment' | 'fulfillment' | 'notification' | 'api-gateway'
  throughput    REAL    NOT NULL,           -- messages processed per second in this window
  error_rate    REAL    NOT NULL,           -- 0.0–1.0 fraction of messages that errored
  p50_latency   REAL    NOT NULL,           -- ms
  p99_latency   REAL    NOT NULL,           -- ms
  active_faults TEXT    NOT NULL DEFAULT '' -- comma-separated fault names active at write time
);

CREATE INDEX idx_service_metrics_ts      ON service_metrics (ts);
CREATE INDEX idx_service_metrics_service ON service_metrics (service, ts);
```

### `queue_metrics`

Written by each queue every 2–3 seconds.

```sql
CREATE TABLE queue_metrics (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  ts          INTEGER NOT NULL,   -- Unix ms timestamp
  queue       TEXT    NOT NULL,   -- 'order-queue' | 'payment-queue' | 'payment-dlq' | 'notification-dlq'
  depth       INTEGER NOT NULL,   -- current number of messages in queue
  enqueue_rate REAL   NOT NULL,   -- messages enqueued per second in this window
  dequeue_rate REAL   NOT NULL,   -- messages dequeued per second in this window
  dlq         INTEGER NOT NULL DEFAULT 0  -- 1 if this is a dead-letter queue
);

CREATE INDEX idx_queue_metrics_ts    ON queue_metrics (ts);
CREATE INDEX idx_queue_metrics_queue ON queue_metrics (queue, ts);
```

### `incidents`

Written by the monitor's correlation engine when a correlated failure is detected.

```sql
CREATE TABLE incidents (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at      INTEGER NOT NULL,           -- Unix ms of first anomaly signal
  resolved_at     INTEGER,                    -- Unix ms, NULL if still active
  severity        TEXT    NOT NULL,           -- 'warning' | 'critical'
  root_cause      TEXT    NOT NULL,           -- human-readable inferred cause
  root_service    TEXT    NOT NULL,           -- service where the fault originated
  affected        TEXT    NOT NULL,           -- JSON array of affected service names
  fault_type      TEXT,                       -- matched fault name if identified
  timeline        TEXT    NOT NULL,           -- JSON array of TimelineEvent objects
  status          TEXT    NOT NULL DEFAULT 'active'  -- 'active' | 'resolved'
);

CREATE INDEX idx_incidents_started_at ON incidents (started_at);
CREATE INDEX idx_incidents_status     ON incidents (status);
```

#### `TimelineEvent` JSON shape (stored in `incidents.timeline`)

```json
[
  {
    "ts": 1710000000000,
    "service": "payment",
    "event": "error_rate_spike",
    "value": 0.84,
    "note": "Error rate rose from 0.02 to 0.84 over 4s"
  },
  {
    "ts": 1710000004000,
    "service": "payment-dlq",
    "event": "dlq_fill",
    "value": 47,
    "note": "DLQ depth climbed from 0 to 47 messages"
  }
]
```

### `fault_events`

Written by the fault engine when a fault is activated or deactivated. Used by the correlation engine to cross-reference timing.

```sql
CREATE TABLE fault_events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  ts         INTEGER NOT NULL,   -- Unix ms
  fault_type TEXT    NOT NULL,   -- 'poison-pill' | 'downstream-timeout' | etc.
  action     TEXT    NOT NULL,   -- 'start' | 'stop'
  origin     TEXT    NOT NULL    -- service name where the fault was injected
);
```

---

## Fault Engine

The fault engine is a singleton that lives inside the simulator process. Services hold a reference to it and poll it on each processing tick to determine their current behavior.

### Interface

```
FaultEngine {
  activate(fault: FaultType, origin: ServiceName): void
  deactivate(fault: FaultType): void
  getActiveFaults(): ActiveFault[]
  isFaultActive(fault: FaultType): boolean
  getFaultContext(service: ServiceName): FaultContext
}

FaultContext {
  shouldDropMessage: boolean       // for poison pill simulation
  processingDelayMs: number        // for timeout simulation (0 = normal)
  shouldCrash: boolean             // for cascading failure
  errorProbability: number         // 0.0–1.0 for intermittent errors
  throughputMultiplier: number     // >1.0 for traffic spike at gateway
}
```

### How a Fault Modifies Service Behavior

Each service worker calls `engine.getFaultContext(this.name)` at the start of each processing tick. The context object tells it exactly what behavior to exhibit. The fault engine maps fault types to context shapes:

| Fault | Affected Services | Context Shape |
|---|---|---|
| `poison-pill` | order → payment | `order: { shouldDropMessage: false }`, `payment: { errorProbability: 1.0 }` |
| `downstream-timeout` | payment | `payment: { processingDelayMs: 8000 }` |
| `traffic-spike` | api-gateway | `api-gateway: { throughputMultiplier: 10.0 }` |
| `cascading-failure` | payment | `payment: { shouldCrash: true }` |
| `intermittent-errors` | all | `*: { errorProbability: 0.15 }` |

The engine writes a `fault_events` row on `activate()` and `deactivate()`. This timestamp anchor is what the correlation engine uses to establish causal ordering.

---

## Correlation Engine

The correlation engine runs inside the monitor process on each polling cycle. It does not use ML — it uses a rule-based approach with three phases:

### Phase 1: Anomaly Detection

For each service and queue, the monitor maintains a rolling baseline window of the last 60 seconds of metric rows. An anomaly is flagged when:

- `error_rate` exceeds `baseline_mean + 3 * baseline_stddev` (minimum threshold: 0.10)
- `queue_depth` exceeds `baseline_mean + 2 * baseline_stddev` (minimum threshold: 20 messages)
- `p99_latency` exceeds `baseline_mean + 3 * baseline_stddev` (minimum threshold: 500ms)
- `dlq_depth` exceeds 0 (any DLQ activity is an anomaly by definition)

Anomaly signals are not immediately written to `incidents`. They enter a candidate buffer.

### Phase 2: Correlation

The engine examines the candidate buffer for signals that arrive within a 10-second correlation window. If two or more anomaly signals appear within this window across different services, they are treated as a correlated event group.

The engine then applies the topology graph to determine causal ordering:

```
api-gateway → order → payment → fulfillment
                           └──→ notification
```

Within a correlated group, the anomaly signal that appears **earliest** in the topology — or **first in time** among signals at the same topology depth — is nominated as the root cause.

#### Example: Poison Pill Correlation

1. `t=0`: payment `error_rate` → 0.84 (anomaly flagged)
2. `t=4s`: payment-dlq `depth` → 47 (anomaly flagged)
3. `t=6s`: fulfillment `throughput` → 0.1 (starvation, anomaly flagged)

Correlation window groups all three. Topology says payment is upstream of fulfillment. DLQ activity is a consequence of payment errors. Root cause nominated: payment service error spike. Cross-reference against `fault_events` table: a `poison-pill` fault was activated at `t=-2s` on the `order` service. Engine revises root cause to: order service poison pill → payment processing failures.

### Phase 3: Incident Construction

Once a correlated group is identified and a root cause nominated, the engine writes an `incidents` row with:

- `started_at`: timestamp of the earliest anomaly signal
- `root_cause`: human-readable string composed from the fault type and topology
- `root_service`: the nominated origin service
- `affected`: all services with anomaly signals in the correlation window
- `timeline`: ordered array of `TimelineEvent` objects, one per anomaly signal
- `severity`: `critical` if any service has `shouldCrash` or DLQ depth > 50; `warning` otherwise

An incident is resolved when all anomaly signals in its affected set return to within 1.5 standard deviations of their baselines for two consecutive polling cycles. The `resolved_at` timestamp is written and `status` is set to `resolved`.

---

## Key Design Decisions

### SQLite over Postgres

SQLite is sufficient for the write volume (5 services × 1 write/2s = ~2.5 writes/second). It eliminates an external dependency from the dev setup, making `pnpm dev` a one-command start. For a portfolio project, reducing the "time to first demo" is a real priority. If this were a production system handling dozens of services, the query patterns (time-series range scans, rolling window aggregations) would favor a purpose-built TSDB like InfluxDB or Prometheus.

### In-Process Fakes over Real AWS SDK

Real AWS services introduce latency, cost, credentials, and region concerns that are all orthogonal to what this project is demonstrating. The fake implementations match the SDK interfaces (SQS `sendMessage`, `receiveMessage`, CloudWatch `putMetricData`) so they can be swapped for real clients without changing service logic. The fakes are clearly documented as fakes — this is a feature, not a shortcut.

### Polling over Event-Driven Architecture

The monitor polls SQLite rather than subscribing to a change feed. This keeps the architecture linear and easy to reason about. The 2-second polling interval means the UI is at most 2 seconds behind reality, which is acceptable for a monitoring demo. SQLite does not have native pub/sub, and implementing a file-watcher workaround would add complexity without meaningfully improving the demo experience.

### SSE over WebSocket for Real-Time Dashboard

The dashboard receives updates via Server-Sent Events rather than WebSockets. SSE is unidirectional (server → client), which matches the data flow perfectly — the dashboard never needs to push data to the monitor. SSE is simpler to implement, works over standard HTTP/2, and requires no upgrade handshake. WebSockets are the right choice when the client needs to send data; they are unnecessary overhead here.
