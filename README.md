# Watchtower

A lightweight AWS microservice monitoring tool with a built-in fault simulation environment. Built to demonstrate what a production observability layer could look like — without requiring any real AWS infrastructure.

Watchtower simulates a realistic e-commerce microservice topology, lets you inject named fault scenarios via a UI button, and shows you in real time how failures propagate, correlate, and resolve. The correlation engine infers root cause from metric signals and reconstructs a human-readable incident timeline.

---

## Why This Exists

Most monitoring demos are either trivially simple (one service, one metric) or require a complex cloud environment to run. Watchtower is designed to hit a useful middle ground:

- The simulated system is realistic enough to generate interesting failure modes
- The fault scenarios are the actual failure patterns that matter in production (poison pills, cascading failures, DLQ saturation)
- The correlation engine does real work — it doesn't just alert on thresholds, it reasons about causality
- The entire thing runs locally with no cloud account required

This project is intended as a portfolio demonstration for engineers evaluating distributed systems and observability work.

---

## How to Run

> Prerequisites: Go 1.22+, Node.js 20+, pnpm 8+

```bash
# Clone the repo
git clone https://github.com/your-username/watchtower.git
cd watchtower

# Install dashboard dependencies
pnpm install

# Start the simulator (generates metrics, writes to SQLite)
go run ./cmd/simulator

# In a second terminal, start the monitor
go run ./cmd/monitor

# In a third terminal, start the dashboard
pnpm --filter dashboard dev

# Or, run everything together
make dev
```

The dashboard will be available at `http://localhost:3000`.

The simulator starts automatically. Click **Inject Fault** in the dashboard to trigger a named failure scenario and watch the system respond.

---

## Screenshots

> _Screenshots will be added once the dashboard is built._

| View | Description |
|---|---|
| `docs/screenshots/dashboard-idle.png` | All services healthy, queues at steady-state depth |
| `docs/screenshots/dashboard-poison-pill.png` | Payment DLQ filling, Fulfillment starved |
| `docs/screenshots/dashboard-incident-panel.png` | Incident timeline with inferred root cause |
| `docs/screenshots/fault-injection-ui.png` | Fault selector with active fault indicator |

---

## Tech Stack

| Layer | Choice |
|---|---|
| Simulator & Monitor | Go 1.22+, `modernc.org/sqlite` (pure Go, no CGO) |
| Frontend | Next.js 14+, TypeScript, Tailwind CSS |
| Storage | SQLite (`watchtower.db`) — shared between simulator and monitor |
| Charts | Recharts |
| Real-time updates | Server-Sent Events (SSE) |
| AWS SDK | In-process fakes (no real AWS required) |

---

## Simulated Topology

```
API Gateway
     │
     ▼
Order Service ──[order-queue]──▶ Payment Service ──[payment-queue]──▶ Fulfillment Service
                                        │
                                [payment-dlq]        ──▶ Notification Service
                                                              │
                                                     [notification-dlq]
```

Five services, four queues, two DLQs. Each service is an in-process worker. Each queue is an in-memory structure that writes metrics to a shared SQLite database every 2–3 seconds.

---

## Fault Scenarios

| Fault | Trigger Point | What You'll See |
|---|---|---|
| Poison pill | Order Service | Payment errors spike → DLQ fills → Fulfillment depth drops to zero |
| Downstream timeout | Payment Service | Order queue depth climbs → API Gateway P99 rises |
| Traffic spike | API Gateway | All queue depths climb in unison, throughput saturates |
| Cascading failure | Payment Service crash | Fulfillment starves, Notification DLQ accumulates |
| Random intermittent errors | Random services (~15% rate) | Noisy signal across services, tests dedup and grouping |

---

## Planning Documents

| Document | Contents |
|---|---|
| [Architecture](docs/architecture.md) | System design, SQLite schema, correlation engine, design decisions |
| [Order Service Build Plan](docs/build-order-service.md) | Queue interface, metrics emitted, fault behavior, acceptance criteria |
| [Payment Service Build Plan](docs/build-payment-service.md) | Timeout handling, DLQ logic, retry simulation, acceptance criteria |
| [Fulfillment Service Build Plan](docs/build-fulfillment-service.md) | Starvation behavior, throughput metrics, acceptance criteria |
| [Notification Service Build Plan](docs/build-notification-service.md) | DLQ accumulation, silent failure patterns, acceptance criteria |
| [Monitor Build Plan](docs/build-monitor.md) | Polling loop, anomaly detection, correlation engine, incident model |
| [Dashboard Build Plan](docs/build-dashboard.md) | Component structure, charts, fault injection UI, real-time strategy |
