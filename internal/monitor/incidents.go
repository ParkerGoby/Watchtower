package monitor

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Incident mirrors the incidents table row.
type Incident struct {
	ID          int64           `json:"id"`
	StartedAt   int64           `json:"startedAt"`
	ResolvedAt  *int64          `json:"resolvedAt"`
	Severity    string          `json:"severity"`
	RootCause   string          `json:"rootCause"`
	RootService string          `json:"rootService"`
	Affected    []string        `json:"affected"`
	FaultType   *string         `json:"faultType"`
	Timeline    []TimelineEvent `json:"timeline"`
	Status      string          `json:"status"`
}

// IncidentStore handles incident persistence against SQLite.
type IncidentStore struct {
	db *sql.DB
}

// NewIncidentStore creates an IncidentStore backed by db.
func NewIncidentStore(db *sql.DB) *IncidentStore {
	return &IncidentStore{db: db}
}

// Reconcile creates or updates incidents based on a batch of CorrelatedEvents.
// Returns the full current set of active incidents.
func (s *IncidentStore) Reconcile(events []CorrelatedEvent) ([]Incident, error) {
	actives, err := s.ListActive()
	if err != nil {
		return nil, fmt.Errorf("list active: %w", err)
	}

	for _, ev := range events {
		matched := findMatchingIncident(actives, ev)
		if matched == nil {
			if err := s.create(ev); err != nil {
				return nil, fmt.Errorf("create incident: %w", err)
			}
		} else {
			if err := s.appendTimeline(matched.ID, ev.Timeline); err != nil {
				return nil, fmt.Errorf("update timeline: %w", err)
			}
		}
	}

	return s.ListActive()
}

// MarkResolved marks an incident as resolved at the given timestamp.
func (s *IncidentStore) MarkResolved(id int64, resolvedAt int64) error {
	_, err := s.db.Exec(
		`UPDATE incidents SET status='resolved', resolved_at=? WHERE id=?`,
		resolvedAt, id,
	)
	return err
}

// ListActive returns all incidents with status='active'.
func (s *IncidentStore) ListActive() ([]Incident, error) {
	return s.query(`WHERE status='active' ORDER BY started_at DESC`)
}

// ListAll returns all incidents ordered by started_at DESC with pagination.
func (s *IncidentStore) ListAll(limit, offset int) ([]Incident, error) {
	return s.query(fmt.Sprintf(`ORDER BY started_at DESC LIMIT %d OFFSET %d`, limit, offset))
}

// GetByID returns a single incident or nil if not found.
func (s *IncidentStore) GetByID(id int64) (*Incident, error) {
	rows, err := s.query(fmt.Sprintf(`WHERE id=%d`, id))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func (s *IncidentStore) create(ev CorrelatedEvent) error {
	affected, _ := json.Marshal(ev.Affected)
	timeline, _ := json.Marshal(ev.Timeline)
	var faultType interface{}
	if ev.FaultType != "" {
		faultType = ev.FaultType
	}
	_, err := s.db.Exec(
		`INSERT INTO incidents (started_at, severity, root_cause, root_service, affected, fault_type, timeline, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'active')`,
		ev.StartTS, ev.Severity, ev.RootCause, ev.RootService,
		string(affected), faultType, string(timeline),
	)
	return err
}

func (s *IncidentStore) appendTimeline(id int64, newEvents []TimelineEvent) error {
	var raw string
	err := s.db.QueryRow(`SELECT timeline FROM incidents WHERE id=?`, id).Scan(&raw)
	if err != nil {
		return err
	}
	var existing []TimelineEvent
	_ = json.Unmarshal([]byte(raw), &existing)
	merged := append(existing, newEvents...)
	updated, _ := json.Marshal(merged)
	_, err = s.db.Exec(`UPDATE incidents SET timeline=? WHERE id=?`, string(updated), id)
	return err
}

func (s *IncidentStore) query(clause string) ([]Incident, error) {
	rows, err := s.db.Query(`SELECT id, started_at, resolved_at, severity, root_cause, root_service, affected, fault_type, timeline, status FROM incidents ` + clause)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var incidents []Incident
	for rows.Next() {
		var inc Incident
		var resolvedAt sql.NullInt64
		var faultType sql.NullString
		var affectedJSON, timelineJSON string

		if err := rows.Scan(&inc.ID, &inc.StartedAt, &resolvedAt, &inc.Severity,
			&inc.RootCause, &inc.RootService, &affectedJSON, &faultType, &timelineJSON, &inc.Status); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			inc.ResolvedAt = &resolvedAt.Int64
		}
		if faultType.Valid {
			inc.FaultType = &faultType.String
		}
		_ = json.Unmarshal([]byte(affectedJSON), &inc.Affected)
		_ = json.Unmarshal([]byte(timelineJSON), &inc.Timeline)
		incidents = append(incidents, inc)
	}
	return incidents, rows.Err()
}

// findMatchingIncident finds an active incident matching the event's root_service and fault_type.
func findMatchingIncident(actives []Incident, ev CorrelatedEvent) *Incident {
	for i := range actives {
		inc := &actives[i]
		if inc.RootService != ev.RootService {
			continue
		}
		// Match fault_type if both have one, or if neither has one.
		incFT := ""
		if inc.FaultType != nil {
			incFT = *inc.FaultType
		}
		if incFT == ev.FaultType {
			return inc
		}
	}
	return nil
}

// ResolutionTracker tracks how many consecutive cycles all affected services have been at baseline.
type ResolutionTracker struct {
	// counts maps incident ID → consecutive "at baseline" cycle count
	counts map[int64]int
}

// NewResolutionTracker creates a ResolutionTracker.
func NewResolutionTracker() *ResolutionTracker {
	return &ResolutionTracker{counts: make(map[int64]int)}
}

// Check evaluates each active incident against current anomaly signals.
// Returns IDs of incidents that should now be resolved.
// An incident resolves when it has had 0 active signals for 2 consecutive cycles.
func (rt *ResolutionTracker) Check(actives []Incident, activeSignals []AnomalySignal) []int64 {
	// Build set of currently anomalous services/queues.
	// Queue signals are also attributed to their owning service so that a
	// DLQ-backed incident doesn't resolve prematurely while the DLQ is non-empty.
	anomalousServices := map[string]bool{}
	for _, sig := range activeSignals {
		if sig.Service != "" {
			anomalousServices[sig.Service] = true
		}
		if sig.Queue != "" {
			anomalousServices[sig.Queue] = true
			if owner, ok := queueOwner[sig.Queue]; ok {
				anomalousServices[owner] = true
			}
		}
	}

	var toResolve []int64
	activeIDs := map[int64]bool{}

	for _, inc := range actives {
		activeIDs[inc.ID] = true
		// Check whether any affected service is still anomalous.
		stillAnomalous := false
		for _, svc := range inc.Affected {
			if anomalousServices[svc] {
				stillAnomalous = true
				break
			}
		}
		if stillAnomalous {
			rt.counts[inc.ID] = 0
		} else {
			rt.counts[inc.ID]++
			if rt.counts[inc.ID] >= 2 {
				toResolve = append(toResolve, inc.ID)
			}
		}
	}

	// Clean up counts for incidents that are no longer active.
	for id := range rt.counts {
		if !activeIDs[id] {
			delete(rt.counts, id)
		}
	}

	return toResolve
}

// ServiceStatus summarises the current health of a service for /api/state.
type ServiceStatus struct {
	Name         string   `json:"name"`
	Throughput   float64  `json:"throughput"`
	ErrorRate    float64  `json:"errorRate"`
	P50Latency   float64  `json:"p50Latency"`
	P99Latency   float64  `json:"p99Latency"`
	Status       string   `json:"status"`
	ActiveFaults []string `json:"activeFaults"`
}

// QueueStatus summarises the current health of a queue for /api/state.
type QueueStatus struct {
	Name        string  `json:"name"`
	Depth       float64 `json:"depth"`
	EnqueueRate float64 `json:"enqueueRate"`
	DequeueRate float64 `json:"dequeueRate"`
	IsDLQ       bool    `json:"isDlq"`
}

// StateSnapshot is the payload for GET /api/state and SSE state events.
type StateSnapshot struct {
	TS            int64           `json:"ts"`
	Services      []ServiceStatus `json:"services"`
	Queues        []QueueStatus   `json:"queues"`
	Incidents     []Incident      `json:"incidents"`
	BaselineReady bool            `json:"baselineReady"`
}

// BuildServiceStatus converts a ServiceRow into a ServiceStatus using active signals.
func BuildServiceStatus(row ServiceRow, anomalous map[string]bool) ServiceStatus {
	status := "healthy"
	if anomalous[row.Service] {
		status = "degraded"
	}
	var faults []string
	if row.ActiveFaults != "" {
		for _, f := range strings.Split(row.ActiveFaults, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				faults = append(faults, f)
			}
		}
	}
	if faults == nil {
		faults = []string{}
	}
	return ServiceStatus{
		Name:         row.Service,
		Throughput:   row.Throughput,
		ErrorRate:    row.ErrorRate,
		P50Latency:   row.P50Latency,
		P99Latency:   row.P99Latency,
		Status:       status,
		ActiveFaults: faults,
	}
}

// BuildQueueStatus converts a QueueRow into a QueueStatus.
func BuildQueueStatus(row QueueRow) QueueStatus {
	return QueueStatus{
		Name:        row.Queue,
		Depth:       row.Depth,
		EnqueueRate: row.EnqueueRate,
		DequeueRate: row.DequeueRate,
		IsDLQ:       row.DLQ,
	}
}

// ResolutionRequired is the number of consecutive clean cycles before resolving.
const ResolutionRequired = 2

// nowMS returns current Unix milliseconds.
func nowMS() int64 {
	return time.Now().UnixMilli()
}
