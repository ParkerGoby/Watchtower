# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working in this repository.

---

## Project Overview

Monitower is a full-stack AWS microservice monitoring tool with a built-in fault simulation environment. It models a realistic e-commerce order pipeline and demonstrates how failure propagates, correlates, and resolves across distributed services. There is no real AWS infrastructure — everything is simulated in-process.

**Status:** The project is in the planning phase. All implementation specs live in `docs/`. No source code exists yet. Build in the order: simulator services → monitor → dashboard.

---

## Repo Layout

```
monitower/
  cmd/
    simulator/main.go   -- simulator entry point
    monitor/main.go     -- monitor entry point
  internal/
    db/                 -- shared SQLite schema, migration, query helpers
    simulator/          -- service workers, queue implementations, fault engine
    monitor/            -- polling loop, anomaly detection, correlation engine, HTTP API
  dashboard/            -- Next.js 14+ app (TypeScript)
  go.mod
  go.sum
  Makefile
  package.json          -- root package.json (dashboard workspace only)
```

The Go module root is the repo root. `cmd/simulator` and `cmd/monitor` are the two Go binaries. `dashboard/` is a standalone Next.js app managed via pnpm.

---

## Tech Stack

| Layer | Technology |
|-------|------------|
| Simulator & Monitor | Go 1.22+, `modernc.org/sqlite` (pure Go, no CGO) |
| Frontend | Next.js 14+, TypeScript, Tailwind CSS, Recharts |
| Database | SQLite (`monitower.db`) — shared between simulator and monitor |
| Real-time | Server-Sent Events (SSE) — monitor pushes state snapshots every 2s |
| Package manager | pnpm (dashboard only) |

---

## Commands

```bash
# --- Go (simulator & monitor) ---

# Run simulator
go run ./cmd/simulator

# Run monitor
go run ./cmd/monitor

# Build both binaries
go build ./cmd/simulator ./cmd/monitor

# Run all Go tests
go test ./...

# Run tests for a specific package
go test ./internal/monitor/...

# Run a single test by name
go test ./internal/monitor/... -run TestCorrelator_PoisonPill

# Lint (requires golangci-lint)
golangci-lint run

# Format
gofmt -w .

# --- Dashboard (Next.js) ---

# Install dependencies
pnpm install

# Dev server
pnpm --filter dashboard dev

# Lint
pnpm --filter dashboard lint

# Type check
pnpm --filter dashboard typecheck

# E2E tests (Playwright)
pnpm --filter dashboard test:e2e

# Run a single Playwright test
pnpm --filter dashboard test:e2e -- --grep "<test name>"

# --- Run everything together ---
make dev
```

---

## Architecture

### Data Flow

```
API Gateway (fake)
    ↓
Order Service → [order-queue] → Payment Service → [payment-queue] → Fulfillment Service
                                      ├→ [payment-dlq]
                                      └→ Notification Service → [notification-dlq]
```

Each service is a Go goroutine that writes metrics to SQLite every 2–3 seconds. The monitor runs a ticker every 2 seconds, reads metrics, runs anomaly detection + correlation, and updates the `incidents` table. The dashboard connects to the monitor via SSE.

**IPC is file-based:** simulator and monitor communicate exclusively through `monitower.db`. The monitor is entirely read-only against `service_metrics` and `queue_metrics`.

### SQLite Schema (defined in `internal/db/`)

- **`service_metrics`** — `(id, ts, service, throughput, error_rate, p50_latency, p99_latency, active_faults)`
- **`queue_metrics`** — `(id, ts, queue, depth, enqueue_rate, dequeue_rate, dlq)`
- **`incidents`** — `(id, started_at, resolved_at, severity, root_cause, root_service, affected, fault_type, timeline, status)`
- **`fault_events`** — `(id, ts, fault_type, action, origin)` — written on fault activation/deactivation

Schema is applied by the simulator at startup via a migration in `internal/db/`.

### Correlation Engine (`internal/monitor/`) — 3 phases

1. **Anomaly detection** — 60s rolling baseline per service/queue; flags via mean + 3σ (errors/latency) / mean + 2σ (queue depth); DLQ depth > 0 is always an anomaly.
2. **Correlation** — groups signals in 10s windows; walks topology graph to find causal root; cross-references `fault_events`.
3. **Incident construction** — writes human-readable incidents; resolves when signals return to baseline for 2 consecutive cycles.

### Monitor HTTP API — `net/http` on port 3001

- `GET /api/state` — full system snapshot
- `GET /api/incidents`, `GET /api/incidents/:id`
- `GET /api/metrics/history`
- `GET /api/events/stream` — SSE stream (full state every 2s; immediate `incident` events on change)

### Simulator HTTP API — `net/http` on port 3002

- `POST /api/faults/inject`, `POST /api/faults/stop`, `GET /api/faults/active`
- `POST /api/reset`

The dashboard proxies both APIs through Next.js Route Handlers to avoid CORS.

### Dashboard Layout

Single-page App Router app. Three-column layout at `lg` breakpoint: service health grid | incident panel | fault injector. Real-time state managed via a single `useMonitowerState` hook that subscribes to the SSE stream.

---

## Documentation Reference

Each feature has a detailed spec in `docs/`. Always read the relevant doc before implementing:

| Doc | Covers |
|-----|--------|
| `docs/architecture.md` | Full schema, correlation engine logic, design decisions |
| `docs/build-order-service.md` | Order Service goroutine, metrics, fault behavior |
| `docs/build-payment-service.md` | Payment Service timeouts, DLQ, retry logic |
| `docs/build-fulfillment-service.md` | Starvation patterns, throughput metrics |
| `docs/build-notification-service.md` | Silent failure, DLQ accumulation |
| `docs/build-monitor.md` | Polling ticker, anomaly detection, correlation, HTTP routes |
| `docs/build-dashboard.md` | Component structure, charts, real-time strategy, fault injection UI |

---

## Workflow Rules

### Branching

- **Always branch off `main`** before starting any feature or fix. Never commit directly to `main`.
- Branch naming: `feat/<name>`, `fix/<name>`, `chore/<name>`

### Commits

- **Never commit code.** Stage changes and let the user commit.

### Linting & Formatting

- Go: run `golangci-lint run` and `gofmt -w .` before considering a feature done. Zero lint errors.
- Dashboard: run `pnpm --filter dashboard lint` and `pnpm --filter dashboard typecheck`. No TypeScript errors.

---

## Test-Driven Development

Follow this sequence for every feature:

1. **Read the spec** — read the relevant `docs/build-*.md` first.
2. **Draft test cases** — write out the test cases you intend to cover and **present them to the user for confirmation** before writing any code.
3. **Write the tests** — implement the confirmed test cases. Tests must fail at this point (red).
4. **Implement the feature** — write the minimum code to make tests pass (green).
5. **Verify** — run tests and confirm they pass. For frontend features, include at least one Playwright test confirming the component renders/behaves correctly in a browser. For Go packages, `go test ./...` must pass.
6. **Lint and format** — run lint and format commands for the relevant layer.
7. **Edge cases** — after the feature passes, reason about potential edge cases. List any relevant ones, note which you believe are significant enough to handle now, and ask the user to confirm before implementing fixes.

---

## Agent Self-Sufficiency

Before surfacing work to the user:

- All tests pass (`go test ./...` and/or Playwright).
- Lint and format are clean.
- At least one test verifies the feature end-to-end (not just isolated unit logic).
- For new API routes: integration test or smoke check that the route responds correctly.
- For new UI components: a Playwright test confirms it renders with real data.

Only prompt the user to review when all of the above are green.
