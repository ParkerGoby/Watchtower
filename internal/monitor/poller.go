package monitor

import (
	"database/sql"
	"log"
	"time"
)

const (
	pollInterval     = 2 * time.Second
	metricReadWindow = 30 * time.Second // read metrics from last 30s
	baselineWindow   = 60 * time.Second
	maxCycleMS       = 1800 // skip next cycle if last took longer than this
)

// Poller is the heartbeat of the monitor. It runs every 2 seconds.
type Poller struct {
	db          *sql.DB
	baselines   *Baselines
	correlator  *Correlator
	incStore    *IncidentStore
	resolver    *ResolutionTracker
	broadcaster *SSEBroadcaster

	// lastSnapshot is the most recently built state snapshot.
	lastSnapshot StateSnapshot
}

// NewPoller creates a Poller wired to all monitor subsystems.
func NewPoller(db *sql.DB, broadcaster *SSEBroadcaster) *Poller {
	return &Poller{
		db:          db,
		baselines:   NewBaselines(baselineWindow),
		correlator:  NewCorrelator(),
		incStore:    NewIncidentStore(db),
		resolver:    NewResolutionTracker(),
		broadcaster: broadcaster,
		lastSnapshot: StateSnapshot{
			Services:  []ServiceStatus{},
			Queues:    []QueueStatus{},
			Incidents: []Incident{},
		},
	}
}

// Start launches the polling loop in a goroutine. It never returns.
func (p *Poller) Start() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastCycleMS int64
	for range ticker.C {
		if lastCycleMS > maxCycleMS {
			log.Printf("monitor: last poll cycle took %dms (>%dms), skipping", lastCycleMS, maxCycleMS)
			lastCycleMS = 0
			continue
		}
		start := time.Now()
		if err := p.tick(); err != nil {
			log.Printf("monitor: poll error: %v", err)
		}
		lastCycleMS = time.Since(start).Milliseconds()
	}
}

// Tick runs one polling cycle (exported for testing).
func (p *Poller) Tick() error {
	return p.tick()
}

// Snapshot returns the most recently computed state snapshot.
func (p *Poller) Snapshot() StateSnapshot {
	return p.lastSnapshot
}

func (p *Poller) tick() error {
	now := time.Now().UnixMilli()
	cutoff := now - metricReadWindow.Milliseconds()

	// 1. Read latest metrics.
	serviceRows, err := queryServiceMetrics(p.db, cutoff)
	if err != nil {
		return err
	}
	queueRows, err := queryQueueMetrics(p.db, cutoff)
	if err != nil {
		return err
	}

	// 2. Update baselines.
	for _, r := range serviceRows {
		p.baselines.AddServiceMetric(r.TS, r.Service, "error_rate", r.ErrorRate)
		p.baselines.AddServiceMetric(r.TS, r.Service, "p99_latency", r.P99Latency)
		p.baselines.AddServiceMetric(r.TS, r.Service, "throughput", r.Throughput)
	}
	for _, r := range queueRows {
		p.baselines.AddQueueMetric(r.TS, r.Queue, "depth", r.Depth)
	}

	// 3. Anomaly detection.
	serviceSigs := DetectServiceAnomalies(serviceRows, p.baselines)
	queueSigs := DetectQueueAnomalies(queueRows, p.baselines)
	allSigs := append(serviceSigs, queueSigs...)

	// 4. Correlation.
	p.correlator.AddSignals(now, allSigs)
	faultEvents, err := queryRecentFaultEvents(p.db, now-10000)
	if err != nil {
		return err
	}
	events := p.correlator.Correlate(faultEvents)

	// 5. Reconcile incidents.
	actives, err := p.incStore.Reconcile(events)
	if err != nil {
		log.Printf("monitor: reconcile error: %v", err)
	}

	// 6. Resolve incidents.
	toResolve := p.resolver.Check(actives, p.correlator.ActiveSignals())
	for _, id := range toResolve {
		if err := p.incStore.MarkResolved(id, now); err != nil {
			log.Printf("monitor: resolve incident %d: %v", id, err)
		}
	}

	// Reload actives after resolution.
	actives, _ = p.incStore.ListActive()

	// 7. Build and broadcast snapshot.
	snap := p.buildSnapshot(now, serviceRows, queueRows, actives)
	p.lastSnapshot = snap
	p.broadcaster.BroadcastState(snap)

	return nil
}

func (p *Poller) buildSnapshot(now int64, serviceRows []ServiceRow, queueRows []QueueRow, actives []Incident) StateSnapshot {
	// Determine which services are currently anomalous.
	anomalous := map[string]bool{}
	for _, sig := range p.correlator.ActiveSignals() {
		if sig.Service != "" {
			anomalous[sig.Service] = true
		}
	}

	// Deduplicate service rows — keep the latest row per service.
	latestService := map[string]ServiceRow{}
	for _, r := range serviceRows {
		if existing, ok := latestService[r.Service]; !ok || r.TS > existing.TS {
			latestService[r.Service] = r
		}
	}
	latestQueue := map[string]QueueRow{}
	for _, r := range queueRows {
		if existing, ok := latestQueue[r.Queue]; !ok || r.TS > existing.TS {
			latestQueue[r.Queue] = r
		}
	}

	var services []ServiceStatus
	for _, r := range latestService {
		services = append(services, BuildServiceStatus(r, anomalous))
	}
	var queues []QueueStatus
	for _, r := range latestQueue {
		queues = append(queues, BuildQueueStatus(r))
	}

	// Baseline is ready when at least one service has a valid baseline.
	baselineReady := false
	knownServices := []string{"order", "payment", "fulfillment", "notification"}
	for _, svc := range knownServices {
		_, _, valid := p.baselines.ServiceMetricStats(svc, "error_rate")
		if valid {
			baselineReady = true
			break
		}
	}

	if actives == nil {
		actives = []Incident{}
	}

	return StateSnapshot{
		TS:            now,
		Services:      services,
		Queues:        queues,
		Incidents:     actives,
		BaselineReady: baselineReady,
	}
}

// --- DB query helpers ---

func queryServiceMetrics(db *sql.DB, sinceTS int64) ([]ServiceRow, error) {
	rows, err := db.Query(
		`SELECT ts, service, throughput, error_rate, p50_latency, p99_latency, active_faults
		 FROM service_metrics WHERE ts >= ? ORDER BY ts ASC`, sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceRow
	for rows.Next() {
		var r ServiceRow
		if err := rows.Scan(&r.TS, &r.Service, &r.Throughput, &r.ErrorRate,
			&r.P50Latency, &r.P99Latency, &r.ActiveFaults); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func queryQueueMetrics(db *sql.DB, sinceTS int64) ([]QueueRow, error) {
	rows, err := db.Query(
		`SELECT ts, queue, depth, enqueue_rate, dequeue_rate, dlq
		 FROM queue_metrics WHERE ts >= ? ORDER BY ts ASC`, sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueRow
	for rows.Next() {
		var r QueueRow
		var dlqInt int
		if err := rows.Scan(&r.TS, &r.Queue, &r.Depth, &r.EnqueueRate, &r.DequeueRate, &dlqInt); err != nil {
			return nil, err
		}
		r.DLQ = dlqInt == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func queryRecentFaultEvents(db *sql.DB, sinceTS int64) ([]FaultEvent, error) {
	rows, err := db.Query(
		`SELECT ts, fault_type, action, origin FROM fault_events WHERE ts >= ? ORDER BY ts ASC`, sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FaultEvent
	for rows.Next() {
		var f FaultEvent
		if err := rows.Scan(&f.TS, &f.FaultType, &f.Action, &f.Origin); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
