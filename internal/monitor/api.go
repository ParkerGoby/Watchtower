package monitor

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Server wires together the Poller, IncidentStore, and SSE broadcaster
// and registers all HTTP routes.
type Server struct {
	poller      *Poller
	incStore    *IncidentStore
	broadcaster *SSEBroadcaster
	mux         *http.ServeMux
}

// NewServer creates the HTTP server and registers routes.
func NewServer(poller *Poller, incStore *IncidentStore, broadcaster *SSEBroadcaster) *Server {
	s := &Server{
		poller:      poller,
		incStore:    incStore,
		broadcaster: broadcaster,
		mux:         http.NewServeMux(),
	}
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/incidents", s.handleIncidents)
	s.mux.HandleFunc("/api/incidents/", s.handleIncidentByID)
	s.mux.HandleFunc("/api/metrics/history", s.handleMetricsHistory)
	s.mux.HandleFunc("/api/events/stream", broadcaster.ServeHTTP)
	return s
}

// Handler returns the http.Handler for use with http.ListenAndServe.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.poller.Snapshot())
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := queryInt(r, "limit", 20)
	offset := queryInt(r, "offset", 0)
	incidents, err := s.incStore.ListAll(limit, offset)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if incidents == nil {
		incidents = []Incident{}
	}
	writeJSON(w, incidents)
}

func (s *Server) handleIncidentByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Path: /api/incidents/<id>
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/incidents/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		// Redirect to list handler.
		s.handleIncidents(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	inc, err := s.incStore.GetByID(id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if inc == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, inc)
}

func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serviceName := r.URL.Query().Get("service")
	windowSec := queryInt(r, "window", 300)
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "throughput"
	}

	nowMs := nowMS()
	sinceTS := nowMs - int64(windowSec)*1000

	points, err := queryMetricsHistory(s.poller, serviceName, metric, sinceTS)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if points == nil {
		points = []MetricPoint{}
	}
	writeJSON(w, points)
}

// MetricPoint is a single (ts, value) point for charting.
type MetricPoint struct {
	TS    int64   `json:"ts"`
	Value float64 `json:"value"`
}

// queryMetricsHistory queries metric history from the DB.
func queryMetricsHistory(poller *Poller, service, metric string, sinceTS int64) ([]MetricPoint, error) {
	var query string
	var args []interface{}

	validServiceMetrics := map[string]bool{
		"throughput": true, "error_rate": true, "p50_latency": true, "p99_latency": true,
	}
	validQueueMetrics := map[string]bool{
		"depth": true, "enqueue_rate": true, "dequeue_rate": true,
	}

	if validServiceMetrics[metric] {
		col := metric
		if service != "" {
			query = `SELECT ts, ` + col + ` FROM service_metrics WHERE service=? AND ts>=? ORDER BY ts ASC`
			args = []interface{}{service, sinceTS}
		} else {
			query = `SELECT ts, ` + col + ` FROM service_metrics WHERE ts>=? ORDER BY ts ASC`
			args = []interface{}{sinceTS}
		}
	} else if validQueueMetrics[metric] {
		col := metric
		if service != "" {
			query = `SELECT ts, ` + col + ` FROM queue_metrics WHERE queue=? AND ts>=? ORDER BY ts ASC`
			args = []interface{}{service, sinceTS}
		} else {
			query = `SELECT ts, ` + col + ` FROM queue_metrics WHERE ts>=? ORDER BY ts ASC`
			args = []interface{}{sinceTS}
		}
	} else {
		return []MetricPoint{}, nil
	}

	rows, err := poller.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricPoint
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.TS, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func queryInt(r *http.Request, key string, def int) int {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
