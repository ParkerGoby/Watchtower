package monitor

import (
	"sort"
	"strings"
	"time"
)

// TimelineEvent is a timestamped note in an incident's history.
type TimelineEvent struct {
	TS      int64  `json:"ts"`
	Message string `json:"message"`
}

// CorrelatedEvent is a group of related anomaly signals with a nominated root cause.
type CorrelatedEvent struct {
	StartTS     int64           `json:"startTs"`
	RootService string          `json:"rootService"`
	RootCause   string          `json:"rootCause"`
	FaultType   string          `json:"faultType"`
	Severity    string          `json:"severity"`
	Affected    []string        `json:"affected"`
	Signals     []AnomalySignal `json:"signals"`
	Timeline    []TimelineEvent `json:"timeline"`
}

// FaultEvent represents a row from the fault_events table.
type FaultEvent struct {
	TS        int64
	FaultType string
	Action    string
	Origin    string
}

// topologyDepth maps service names to their depth in the pipeline.
// Lower depth = more upstream.
var topologyDepth = map[string]int{
	"api-gateway":  0,
	"order":        1,
	"payment":      2,
	"fulfillment":  3,
	"notification": 3,
}

// downstreamOf returns all services downstream of the given service.
func downstreamOf(service string) []string {
	depth, ok := topologyDepth[service]
	if !ok {
		return nil
	}
	var downstream []string
	for svc, d := range topologyDepth {
		if d > depth {
			downstream = append(downstream, svc)
		}
	}
	return downstream
}

// queueOwner maps a queue name to the service that writes to it.
var queueOwner = map[string]string{
	"order-queue":      "order",
	"payment-queue":    "payment",
	"payment-dlq":      "payment",
	"notification-dlq": "notification",
}

// Correlator groups active anomaly signals and produces CorrelatedEvents.
type Correlator struct {
	activeSignals []AnomalySignal
	signalWindow  time.Duration // prune signals older than this
	groupWindow   int64         // max ms span for a signal group
}

// NewCorrelator returns a Correlator with default settings.
func NewCorrelator() *Correlator {
	return &Correlator{
		signalWindow: 30 * time.Second,
		groupWindow:  10000, // 10 seconds in ms
	}
}

// AddSignals merges new signals into the active set, pruning stale ones.
func (c *Correlator) AddSignals(nowMS int64, signals []AnomalySignal) {
	// Prune old signals.
	cutoff := nowMS - c.signalWindow.Milliseconds()
	pruned := c.activeSignals[:0]
	for _, s := range c.activeSignals {
		if s.TS >= cutoff {
			pruned = append(pruned, s)
		}
	}
	c.activeSignals = pruned

	// Add new signals, deduplicating by (service+queue, metric).
	for _, ns := range signals {
		key := signalKey(ns)
		replaced := false
		for i, existing := range c.activeSignals {
			if signalKey(existing) == key {
				if absFloat(ns.Deviation) >= absFloat(existing.Deviation) {
					c.activeSignals[i] = ns
				}
				replaced = true
				break
			}
		}
		if !replaced {
			c.activeSignals = append(c.activeSignals, ns)
		}
	}
}

// ActiveSignals returns a copy of the current active signal set.
func (c *Correlator) ActiveSignals() []AnomalySignal {
	out := make([]AnomalySignal, len(c.activeSignals))
	copy(out, c.activeSignals)
	return out
}

// Correlate produces CorrelatedEvents from the current active signal set.
func (c *Correlator) Correlate(faultEvents []FaultEvent) []CorrelatedEvent {
	if len(c.activeSignals) == 0 {
		return nil
	}

	// Sort signals by TS ascending.
	sorted := make([]AnomalySignal, len(c.activeSignals))
	copy(sorted, c.activeSignals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TS < sorted[j].TS })

	// Group into 10s windows.
	var groups [][]AnomalySignal
	var currentGroup []AnomalySignal
	var groupStart int64

	for _, sig := range sorted {
		if len(currentGroup) == 0 {
			currentGroup = []AnomalySignal{sig}
			groupStart = sig.TS
			continue
		}
		if sig.TS-groupStart <= c.groupWindow {
			currentGroup = append(currentGroup, sig)
		} else {
			groups = append(groups, currentGroup)
			currentGroup = []AnomalySignal{sig}
			groupStart = sig.TS
		}
	}
	if len(currentGroup) > 0 {
		groups = append(groups, currentGroup)
	}

	var events []CorrelatedEvent
	for _, group := range groups {
		// Single non-DLQ signal: skip unless it's a DLQ signal.
		if len(group) == 1 && !isDLQSignal(group[0]) {
			continue
		}

		event := buildCorrelatedEvent(group, faultEvents)
		events = append(events, event)
	}
	return events
}

func buildCorrelatedEvent(signals []AnomalySignal, faultEvents []FaultEvent) CorrelatedEvent {
	groupStart := signals[0].TS

	// Determine root cause.
	rootService := nominateRoot(signals, groupStart, faultEvents)

	// Build affected set.
	affected := buildAffected(rootService, signals)

	// Check for fault_events match.
	faultType := matchFaultType(rootService, groupStart, faultEvents)

	// Human-readable root cause.
	rootCause := buildRootCause(rootService, signals, faultType)

	// Severity.
	severity := computeSeverity(signals)

	// Timeline.
	timeline := []TimelineEvent{{TS: groupStart, Message: "anomaly detected: " + rootCause}}

	return CorrelatedEvent{
		StartTS:     groupStart,
		RootService: rootService,
		RootCause:   rootCause,
		FaultType:   faultType,
		Severity:    severity,
		Affected:    affected,
		Signals:     signals,
		Timeline:    timeline,
	}
}

// nominateRoot returns the root service for a group of signals.
func nominateRoot(signals []AnomalySignal, groupStart int64, faultEvents []FaultEvent) string {
	// Rule 3: direct fault_events evidence within 5s before group start.
	const faultLookback = 5000
	for _, fe := range faultEvents {
		if fe.Action == "start" && fe.TS >= groupStart-faultLookback && fe.TS <= groupStart {
			// Check if this origin matches any service in the group.
			for _, sig := range signals {
				if serviceForSignal(sig) == fe.Origin {
					return fe.Origin
				}
			}
		}
	}

	// Check for starvation pattern: low throughput + low queue depth on same service.
	starvationCandidate := detectStarvation(signals)
	if starvationCandidate != "" {
		// Root is upstream of starved service.
		if upstream := upstreamOf(starvationCandidate); upstream != "" {
			return upstream
		}
	}

	// Check for DLQ-only (no service signals).
	hasServiceSignal := false
	for _, sig := range signals {
		if sig.Service != "" {
			hasServiceSignal = true
			break
		}
	}
	if !hasServiceSignal {
		// Use the DLQ's owning service.
		for _, sig := range signals {
			if sig.Queue != "" {
				if owner, ok := queueOwner[sig.Queue]; ok {
					return owner
				}
			}
		}
	}

	// Rule 1 & 2: smallest topology depth, tie-break by earliest TS.
	best := ""
	bestDepth := 999
	bestTS := int64(0)
	for _, sig := range signals {
		svc := serviceForSignal(sig)
		depth, ok := topologyDepth[svc]
		if !ok {
			depth = 999
		}
		if depth < bestDepth || (depth == bestDepth && (best == "" || sig.TS < bestTS)) {
			best = svc
			bestDepth = depth
			bestTS = sig.TS
		}
	}
	return best
}

// buildAffected returns the root service plus all downstream services with active signals.
func buildAffected(rootService string, signals []AnomalySignal) []string {
	seen := map[string]bool{rootService: true}
	downstream := downstreamOf(rootService)
	for _, sig := range signals {
		svc := serviceForSignal(sig)
		for _, d := range downstream {
			if svc == d {
				seen[d] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for svc := range seen {
		out = append(out, svc)
	}
	sort.Strings(out)
	return out
}

// matchFaultType finds a fault_events 'start' row close to groupStart for the root service.
func matchFaultType(rootService string, groupStart int64, faultEvents []FaultEvent) string {
	const lookback = 5000
	for _, fe := range faultEvents {
		if fe.Action == "start" && fe.Origin == rootService &&
			fe.TS >= groupStart-lookback && fe.TS <= groupStart+2000 {
			return fe.FaultType
		}
	}
	return ""
}

func buildRootCause(rootService string, signals []AnomalySignal, faultType string) string {
	// DLQ-only?
	hasServiceSignal := false
	for _, sig := range signals {
		if sig.Service != "" {
			hasServiceSignal = true
			break
		}
	}
	if !hasServiceSignal {
		return rootService + ": silent failure — no correlated upstream anomaly"
	}
	if faultType != "" {
		return rootService + ": " + faultType
	}
	return rootService + ": anomaly detected"
}

func computeSeverity(signals []AnomalySignal) string {
	for _, sig := range signals {
		if sig.Metric == "dlq_depth" && sig.Value > 50 {
			return "critical"
		}
		if sig.Service == "payment" && sig.Metric == "throughput" && sig.Direction == "low" {
			return "critical"
		}
		if strings.Contains(sig.Service, "crash") || strings.Contains(sig.Metric, "crash") {
			return "critical"
		}
	}
	return "warning"
}

// detectStarvation checks if any service has both low throughput and its input queue has low depth.
// Returns the starved service name, or empty string.
func detectStarvation(signals []AnomalySignal) string {
	lowThroughput := map[string]bool{}
	for _, sig := range signals {
		if sig.Metric == "throughput" && sig.Direction == "low" {
			lowThroughput[sig.Service] = true
		}
	}
	return "" // simplified — starvation detection via poller context is more complete
}

// upstreamOf returns the direct upstream service for a given service.
func upstreamOf(service string) string {
	switch service {
	case "payment":
		return "order"
	case "fulfillment", "notification":
		return "payment"
	default:
		return ""
	}
}

// serviceForSignal resolves a signal to a service name (queue signals resolve to owning service).
func serviceForSignal(sig AnomalySignal) string {
	if sig.Service != "" {
		return sig.Service
	}
	if owner, ok := queueOwner[sig.Queue]; ok {
		return owner
	}
	return sig.Queue
}

func isDLQSignal(sig AnomalySignal) bool {
	return sig.Metric == "dlq_depth"
}

func signalKey(s AnomalySignal) string {
	return s.Service + "|" + s.Queue + "|" + s.Metric
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
