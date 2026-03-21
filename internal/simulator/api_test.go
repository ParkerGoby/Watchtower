package simulator_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parkerg/monitower/internal/simulator"
)

// helper — build a handler and record a request against it.
func doRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else {
		reqBody = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reqBody)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// --- POST /api/faults/inject ---

// 1. Valid fault type → 200, fault becomes active.
func TestSimulatorAPI_Inject_ValidFault(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodPost, "/api/faults/inject",
		`{"faultType":"poison-pill"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !engine.IsFaultActive(simulator.FaultPoisonPill) {
		t.Error("expected poison-pill to be active after inject")
	}
}

// 2. Missing faultType → 400.
func TestSimulatorAPI_Inject_MissingFaultType(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodPost, "/api/faults/inject", `{}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// 3. Unknown fault type string → 400.
func TestSimulatorAPI_Inject_UnknownFaultType(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodPost, "/api/faults/inject",
		`{"faultType":"nuclear-option"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// 4. Fault already active → 200 (no-op).
func TestSimulatorAPI_Inject_AlreadyActive(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultPoisonPill, "order")
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodPost, "/api/faults/inject",
		`{"faultType":"poison-pill"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// --- POST /api/faults/stop ---

// 5. With active faults → 200, all faults deactivated.
func TestSimulatorAPI_Stop_DeactivatesAllFaults(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultPoisonPill, "order")
	engine.Activate(simulator.FaultTrafficSpike, "api-gateway")
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodPost, "/api/faults/stop", `{}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if len(engine.GetActiveFaults()) != 0 {
		t.Error("expected all faults deactivated after stop")
	}
}

// 6. No active faults → 200 (no-op).
func TestSimulatorAPI_Stop_NoActiveFaults(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodPost, "/api/faults/stop", `{}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// --- GET /api/faults/active ---

// 7. No active faults → 200, empty array.
func TestSimulatorAPI_Active_Empty(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodGet, "/api/faults/active", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var result []map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty array, got %v", result)
	}
}

// 8. With active faults → 200, array with type and origin fields.
func TestSimulatorAPI_Active_WithFaults(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultPoisonPill, "order")
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodGet, "/api/faults/active", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var result []map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 active fault, got %d", len(result))
	}
	if result[0]["type"] != "poison-pill" {
		t.Errorf("expected type='poison-pill', got %q", result[0]["type"])
	}
	if result[0]["origin"] == "" {
		t.Error("expected non-empty origin")
	}
}

// --- POST /api/reset ---

// 9. Reset deactivates all active faults → 200.
func TestSimulatorAPI_Reset_DeactivatesAllFaults(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)
	engine.Activate(simulator.FaultPoisonPill, "order")
	engine.Activate(simulator.FaultTrafficSpike, "api-gateway")
	handler := simulator.NewSimulatorHandler(engine)

	rr := doRequest(t, handler, http.MethodPost, "/api/reset", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if len(engine.GetActiveFaults()) != 0 {
		t.Error("expected all faults deactivated after reset")
	}
}
