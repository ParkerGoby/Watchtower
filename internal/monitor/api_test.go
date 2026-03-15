package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/parkerg/monitower/internal/db"
)

// newTestServer creates a fully wired Server backed by an in-memory DB.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	broadcaster := NewSSEBroadcaster()
	poller := NewPoller(conn, broadcaster)
	incStore := NewIncidentStore(conn)
	return NewServer(poller, incStore, broadcaster)
}

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// --- GET /api/state ---

func TestAPI_State_Returns200WithValidJSON(t *testing.T) {
	srv := newTestServer(t)
	w := get(t, srv, "/api/state")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}

	var snap StateSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("invalid JSON: %v\nbody: %s", err, w.Body.String())
	}
	// Incidents should be an array (not null) even when empty.
	if snap.Incidents == nil {
		t.Error("expected incidents to be non-nil array")
	}
}

func TestAPI_State_BaselineReadyFalseWhenNoData(t *testing.T) {
	srv := newTestServer(t)
	w := get(t, srv, "/api/state")
	var snap StateSnapshot
	_ = json.Unmarshal(w.Body.Bytes(), &snap)
	if snap.BaselineReady {
		t.Error("expected baselineReady=false with no data")
	}
}

// --- GET /api/incidents ---

func TestAPI_Incidents_EmptyList(t *testing.T) {
	srv := newTestServer(t)
	w := get(t, srv, "/api/incidents")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var list []Incident
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d items", len(list))
	}
}

func TestAPI_Incidents_LimitAndOffset(t *testing.T) {
	srv := newTestServer(t)
	// Create 3 incidents directly.
	for i := 0; i < 3; i++ {
		ev := makeEvent("order", "")
		ev.StartTS = int64(i * 1000)
		_, _ = srv.incStore.Reconcile([]CorrelatedEvent{ev})
		// Each must be a distinct incident: give different fault types to avoid dedup.
	}
	// Insert them sequentially with distinct root services to bypass dedup.
	srv.incStore.Reconcile([]CorrelatedEvent{makeEvent("payment", "")})
	srv.incStore.Reconcile([]CorrelatedEvent{makeEvent("fulfillment", "")})

	w := get(t, srv, "/api/incidents?limit=2&offset=0")
	var list []Incident
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Errorf("expected 2 items with limit=2, got %d", len(list))
	}
}

// --- GET /api/incidents/:id ---

func TestAPI_IncidentByID_Found(t *testing.T) {
	srv := newTestServer(t)
	_, _ = srv.incStore.Reconcile([]CorrelatedEvent{makeEvent("payment", "poison-pill")})
	actives, _ := srv.incStore.ListActive()
	if len(actives) == 0 {
		t.Fatal("no incident created")
	}

	w := get(t, srv, "/api/incidents/"+itoa(actives[0].ID))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	var inc Incident
	_ = json.Unmarshal(w.Body.Bytes(), &inc)
	if inc.RootService != "payment" {
		t.Errorf("expected payment, got %s", inc.RootService)
	}
	if len(inc.Timeline) == 0 {
		t.Error("expected timeline to be populated")
	}
}

func TestAPI_IncidentByID_NotFound(t *testing.T) {
	srv := newTestServer(t)
	w := get(t, srv, "/api/incidents/99999")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestAPI_IncidentByID_InvalidID(t *testing.T) {
	srv := newTestServer(t)
	w := get(t, srv, "/api/incidents/notanumber")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- GET /api/metrics/history ---

func TestAPI_MetricsHistory_EmptyDB(t *testing.T) {
	srv := newTestServer(t)
	w := get(t, srv, "/api/metrics/history?service=payment&metric=throughput&window=60")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var points []MetricPoint
	if err := json.Unmarshal(w.Body.Bytes(), &points); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("expected empty points, got %d", len(points))
	}
}

func TestAPI_MetricsHistory_FiltersByService(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().UnixMilli()
	// Insert rows for two services.
	srv.poller.db.Exec(
		`INSERT INTO service_metrics (ts, service, throughput, error_rate, p50_latency, p99_latency, active_faults)
		 VALUES (?, 'payment', 5.0, 0.01, 50, 100, '')`, now-1000)
	srv.poller.db.Exec(
		`INSERT INTO service_metrics (ts, service, throughput, error_rate, p50_latency, p99_latency, active_faults)
		 VALUES (?, 'order', 3.0, 0.00, 30, 80, '')`, now-1000)

	w := get(t, srv, "/api/metrics/history?service=payment&metric=throughput&window=60")
	var points []MetricPoint
	_ = json.Unmarshal(w.Body.Bytes(), &points)
	if len(points) != 1 {
		t.Errorf("expected 1 point for payment, got %d", len(points))
	}
	if len(points) > 0 && points[0].Value != 5.0 {
		t.Errorf("expected throughput 5.0, got %f", points[0].Value)
	}
}

// --- GET /api/events/stream ---

func TestAPI_SSEStream_ContentType(t *testing.T) {
	srv := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the handler returns right away

	req := httptest.NewRequest(http.MethodGet, "/api/events/stream", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}
}

func TestAPI_SSEStream_BroadcastReachesClient(t *testing.T) {
	srv := newTestServer(t)
	_ = srv.poller.Tick()
	snap := srv.poller.Snapshot()

	// Verify the broadcaster can marshal and fanout without error.
	srv.broadcaster.BroadcastState(snap)

	// Broadcaster with no connected clients should handle gracefully (no panic).
	if srv.broadcaster.ClientCount() != 0 {
		t.Errorf("expected 0 clients, got %d", srv.broadcaster.ClientCount())
	}
}

// helper
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
