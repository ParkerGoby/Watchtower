# Build Plan: Dashboard

## Overview

The dashboard is a Next.js 14+ application (App Router) that renders real-time system state from the monitor's HTTP/SSE API. It is the primary user-facing surface of Monitower and the thing an evaluator will interact with during a demo.

Design principles:
- **Dense but readable** — show everything relevant without requiring navigation
- **Live without lag** — state updates should feel immediate, not polled
- **Demo-optimized** — fault injection is one click from the main view
- **Code-legible** — component structure should be obvious to an engineer reading the source

---

## Real-Time Update Strategy: Server-Sent Events

The dashboard uses **Server-Sent Events (SSE)** via a Next.js Route Handler (`/api/events/stream`) that proxies the monitor's SSE stream.

**Why SSE over alternatives:**

| Option | Verdict |
|---|---|
| SSE | Chosen. Unidirectional server → client, HTTP/2 native, no upgrade handshake, works with Next.js Route Handlers, dead simple to implement |
| WebSocket | Overkill. The client never sends data. A WebSocket connection here would be using 10% of its capability. |
| Polling (`setInterval` + `fetch`) | Acceptable fallback but introduces 2–5s perceived lag and hammers the API with N clients × N polls/s |
| React Query / SWR | Good for cache management but still polling under the hood — same lag problem |

The SSE stream delivers a full state snapshot every 2 seconds and an immediate `incident` event when incident state changes. React state is updated directly from the event handler; no polling, no cache invalidation logic needed.

**Implementation pattern:**

```typescript
// hooks/useMonitowerState.ts
useEffect(() => {
  const es = new EventSource('/api/events/stream')
  es.addEventListener('state', (e) => setState(JSON.parse(e.data)))
  es.addEventListener('incident', (e) => setIncidents(JSON.parse(e.data)))
  return () => es.close()
}, [])
```

---

## Charting Library: Recharts

**Recharts** is chosen over alternatives for the following reasons:

| Library | Notes |
|---|---|
| Recharts | Chosen. React-native, composable API, good TypeScript support, small bundle impact, easy to customize with Tailwind-compatible stroke colors |
| Victory | More opinionated, larger bundle, less composable |
| Chart.js (via react-chartjs-2) | Canvas-based (not SVG), harder to style with Tailwind, imperative API leaks through the wrapper |
| D3 | Maximum flexibility but requires manual React integration; too much ceremony for sparklines and bar charts |
| Tremor | High-level component library built on Recharts — considered, but adding the abstraction layer reduces legibility for an evaluator reading the source |

Charts used:
- `<LineChart>` / `<AreaChart>` — for metric history sparklines
- `<BarChart>` — for queue depth comparisons
- All charts use the `<ResponsiveContainer>` wrapper for layout adaptability

---

## Page and Route Structure

```
app/
  layout.tsx               -- root layout; dark background, monospace font, SSE provider
  page.tsx                 -- main dashboard (the only page)
  api/
    events/
      stream/
        route.ts           -- proxies monitor SSE stream to browser client
    state/
      route.ts             -- proxies GET /api/state from monitor
    incidents/
      route.ts             -- proxies GET /api/incidents from monitor

components/
  dashboard/
    ServiceGrid.tsx        -- 2-column grid of ServiceCard components
    ServiceCard.tsx        -- per-service metrics card
    QueueDepthBar.tsx      -- compact horizontal bar showing queue depth
    MetricSparkline.tsx    -- small line chart for a single metric over time
    DLQBadge.tsx           -- red count badge for DLQ depth
    IncidentPanel.tsx      -- right-hand panel showing active and recent incidents
    IncidentCard.tsx       -- single incident summary card
    IncidentTimeline.tsx   -- ordered timeline of events within one incident
    FaultInjector.tsx      -- fault injection button and selector UI
    FaultStatusIndicator.tsx -- live indicator showing currently active fault
    SystemStatusBar.tsx    -- top bar with overall system health, uptime, active faults

hooks/
  useMonitowerState.ts    -- SSE connection, state management
  useMetricHistory.ts      -- fetches metric history for sparkline data
```

---

## Layout

The main page uses a three-column layout at `lg` breakpoint, collapsing to single column on smaller screens:

```
┌─────────────────────────────────────────────────────────────────────┐
│  SystemStatusBar — overall health pill, active fault indicator       │
├──────────────────────────────────┬──────────────────────────────────┤
│                                  │                                   │
│  ServiceGrid (2 cols)            │  IncidentPanel                    │
│  ┌──────────┐  ┌──────────┐     │  ┌────────────────────────────┐  │
│  │ Order    │  │ Payment  │     │  │ Active Incidents (if any)  │  │
│  │ Service  │  │ Service  │     │  ├────────────────────────────┤  │
│  └──────────┘  └──────────┘     │  │ Recent Incidents           │  │
│  ┌──────────┐  ┌──────────┐     │  │ (last 5)                   │  │
│  │ Fulfill  │  │ Notif    │     │  └────────────────────────────┘  │
│  │ ment     │  │ Service  │     │                                   │
│  └──────────┘  └──────────┘     │  FaultInjector                   │
│                                  │  ┌────────────────────────────┐  │
│                                  │  │ [Inject Fault ▼]  [▶ Go]   │  │
│                                  │  └────────────────────────────┘  │
└──────────────────────────────────┴──────────────────────────────────┘
```

---

## ServiceCard Component

Each service gets a card showing its current state at a glance. The card has two states: **healthy** (subdued border) and **anomalous** (amber or red border with animated pulse on the status dot).

### Card Contents

| Section | Contents |
|---|---|
| Header | Service name, status dot (green/amber/red), active fault chip (if any) |
| Metrics row | Throughput (msg/s), Error rate (%), P50 latency (ms), P99 latency (ms) — four stat chips in a row |
| DLQ badge | Red badge with DLQ depth, shown only when depth > 0. For services with no DLQ (Order, Fulfillment), hidden. |
| Queue bar | `QueueDepthBar` showing the depth and enqueue/dequeue rates for the service's outbound queue |
| Sparkline | `MetricSparkline` showing the last 60 seconds of `error_rate` as an area chart. Chart height: 40px. |

**Status dot color logic:**

```
green  → error_rate < 0.05 AND no active faults AND no DLQ activity
amber  → error_rate 0.05–0.20 OR p99_latency > 500ms OR DLQ depth 1–10
red    → error_rate > 0.20 OR p99_latency > 2000ms OR DLQ depth > 10 OR service crashed
```

### QueueDepthBar

A horizontal stacked bar component. Width represents depth relative to a maximum scale (configurable per queue, default 200 messages). Color encodes urgency:

- Gray: 0–25% of max
- Yellow: 25–60% of max
- Red: >60% of max

DLQ bars are always shown in red regardless of depth (any DLQ activity is red).

Small text below the bar: `↑ 4.2/s  ↓ 4.1/s` (enqueue/dequeue rates).

---

## IncidentPanel Component

The right-hand panel has two sections:

### Active Incidents

If there are active incidents, each is shown as an `IncidentCard` with:
- Severity badge (warning = yellow, critical = red, animated pulse)
- Root cause text (from `incidents.root_cause`)
- Affected services as chips
- Time since incident started (`"3 minutes ago"`)
- "View Timeline" button that expands the `IncidentTimeline` inline

### IncidentTimeline

An ordered list of `TimelineEvent` objects from the incident. Each event shows:
- Relative time from incident start (e.g., `+00:04`)
- Service name chip
- Event type label (e.g., "Error Rate Spike", "DLQ Fill", "Service Crash")
- Value annotation (e.g., "84% error rate", "47 messages in DLQ")
- Note text (the human-readable description from the event)

The timeline is displayed chronologically. The root cause event is highlighted with a different left-border color (red or amber) to draw attention to it.

### Recent Incidents

Below the active incident section, a compact list of the last 5 resolved incidents:
- Incident ID (short hash of the `id`)
- Root cause (truncated to 60 chars)
- Duration (e.g., "resolved after 2m 14s")
- Fault type chip (if known)

---

## FaultInjector Component

This is the demo's primary interactive element. It lives in the right-hand panel below the incident section.

### Layout

```
┌──────────────────────────────────────────────────────┐
│  Inject Fault                                        │
│                                                      │
│  ┌────────────────────────────────┐  ┌────────────┐  │
│  │ Poison Pill                  ▼ │  │  Inject ▶  │  │
│  └────────────────────────────────┘  └────────────┘  │
│                                                      │
│  ● Fault active: Poison Pill   [Stop Fault ■]        │
│    Injected 14s ago                                  │
└──────────────────────────────────────────────────────┘
```

### Fault Selector

A `<select>` styled with Tailwind. Options:

| Value | Label |
|---|---|
| `poison-pill` | Poison Pill — malformed payload from Order |
| `downstream-timeout` | Downstream Timeout — Payment hangs |
| `traffic-spike` | Traffic Spike — 10x request rate |
| `cascading-failure` | Cascading Failure — Payment crashes |
| `intermittent-errors` | Intermittent Errors — ~15% random failures |

### Inject Button

- Enabled when no fault is currently active
- Calls `POST /api/faults/inject` with `{ faultType }` body (this route is served by the simulator, not the monitor — the monitor is read-only)
- Disabled and shows a spinner while the request is in flight

### FaultStatusIndicator

Shown only when a fault is active:
- Animated red dot + fault name
- Relative time since injection ("14s ago")
- "Stop Fault" button that calls `POST /api/faults/stop`

The active fault state is derived from `useMonitowerState` — the SSE `state` event includes `activeFaults: string[]`. No separate polling needed.

---

## SystemStatusBar

A full-width bar at the top of the page:

```
● Monitower    All systems healthy    5 services    4 queues    ↑ 2m 34s
```

When a fault is active or an incident exists:
```
⚠ Monitower    1 active incident    Cascading Failure active    ↑ 4m 12s
```

Color transitions: green → amber → red based on the most severe active incident or active fault.

---

## Fault Injection API (Simulator HTTP Server)

The simulator exposes a small HTTP server on port **3002** for receiving fault commands from the dashboard. This is the only write path in the system; the monitor has no write API.

| Route | Method | Body | Description |
|---|---|---|---|
| `/api/faults/inject` | POST | `{ faultType: string }` | Activate a named fault |
| `/api/faults/stop` | POST | `{}` | Deactivate the current fault |
| `/api/faults/active` | GET | — | Returns currently active fault(s) |
| `/api/reset` | POST | `{}` | Stop all faults, reset simulator to initial state |

The Next.js dashboard proxies these routes via Route Handlers to avoid CORS issues and keep the simulator port internal.

---

## Acceptance Criteria

| Scenario | Observable outcome |
|---|---|
| Initial load | All service cards render within 500ms; baseline sparklines show 30s of history; no layout shift after SSE connects |
| SSE connected | Service card metrics update visibly within 3 seconds of a simulator metric write |
| Fault injected | Status indicator appears within one SSE cycle (≤2s); service cards begin showing anomaly coloring within 6s |
| Incident created | IncidentCard appears in panel with correct root cause and affected services; timeline populates |
| DLQ badge | Red badge on Notification card when `notification-dlq` depth > 0; badge count matches DB value |
| Fault stopped | Status indicator disappears; service cards begin returning to green; incident resolves and moves to "Recent" section |
| Responsive layout | ServiceGrid collapses to 1 column on viewport < 768px; IncidentPanel stacks below |
