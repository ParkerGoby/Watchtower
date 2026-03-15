package simulator_test

import (
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/parkerg/monitower/internal/db"
	"github.com/parkerg/monitower/internal/simulator"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	f, err := os.CreateTemp("", "monitower-test-*.db")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	conn, err := db.Open(f.Name())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func countFaultEvents(t *testing.T, conn *sql.DB, faultType, action string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM fault_events WHERE fault_type=? AND action=?`,
		faultType, action,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count fault_events: %v", err)
	}
	return n
}

// 1. Activate writes a fault_events row with action='start' and correct origin.
func TestFaultEngine_Activate_WritesFaultEvent(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	engine.Activate(simulator.FaultPoisonPill, "order")

	if got := countFaultEvents(t, conn, "poison-pill", "start"); got != 1 {
		t.Errorf("expected 1 start event, got %d", got)
	}
	var origin string
	conn.QueryRow(`SELECT origin FROM fault_events WHERE fault_type='poison-pill' AND action='start'`).Scan(&origin)
	if origin != "order" {
		t.Errorf("expected origin='order', got %q", origin)
	}
}

// 2. Deactivate writes a fault_events row with action='stop'.
func TestFaultEngine_Deactivate_WritesFaultEvent(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	engine.Activate(simulator.FaultPoisonPill, "order")
	engine.Deactivate(simulator.FaultPoisonPill)

	if got := countFaultEvents(t, conn, "poison-pill", "stop"); got != 1 {
		t.Errorf("expected 1 stop event, got %d", got)
	}
}

// 3. IsFaultActive returns true after activate, false after deactivate.
func TestFaultEngine_IsFaultActive(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	if engine.IsFaultActive(simulator.FaultPoisonPill) {
		t.Error("expected inactive before activation")
	}
	engine.Activate(simulator.FaultPoisonPill, "order")
	if !engine.IsFaultActive(simulator.FaultPoisonPill) {
		t.Error("expected active after activation")
	}
	engine.Deactivate(simulator.FaultPoisonPill)
	if engine.IsFaultActive(simulator.FaultPoisonPill) {
		t.Error("expected inactive after deactivation")
	}
}

// 4. GetActiveFaults returns all active faults, none after all deactivated.
func TestFaultEngine_GetActiveFaults(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	if len(engine.GetActiveFaults()) != 0 {
		t.Error("expected no active faults initially")
	}
	engine.Activate(simulator.FaultPoisonPill, "order")
	engine.Activate(simulator.FaultTrafficSpike, "api-gateway")

	faults := engine.GetActiveFaults()
	if len(faults) != 2 {
		t.Errorf("expected 2 active faults, got %d", len(faults))
	}

	engine.Deactivate(simulator.FaultPoisonPill)
	engine.Deactivate(simulator.FaultTrafficSpike)
	if len(engine.GetActiveFaults()) != 0 {
		t.Error("expected no active faults after deactivating all")
	}
}

// 5. poison-pill: payment service gets errorProbability=1.0.
func TestFaultEngine_GetFaultContext_PoisonPill(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	engine.Activate(simulator.FaultPoisonPill, "order")

	ctx := engine.GetFaultContext("payment")
	if ctx.ErrorProbability != 1.0 {
		t.Errorf("expected errorProbability=1.0, got %f", ctx.ErrorProbability)
	}
}

// 6. downstream-timeout: payment service gets processingDelayMs=8000.
func TestFaultEngine_GetFaultContext_DownstreamTimeout(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	engine.Activate(simulator.FaultDownstreamTimeout, "payment")

	ctx := engine.GetFaultContext("payment")
	if ctx.ProcessingDelayMs != 8000 {
		t.Errorf("expected processingDelayMs=8000, got %d", ctx.ProcessingDelayMs)
	}
}

// 7. traffic-spike: api-gateway gets throughputMultiplier=10.0.
func TestFaultEngine_GetFaultContext_TrafficSpike(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	engine.Activate(simulator.FaultTrafficSpike, "api-gateway")

	ctx := engine.GetFaultContext("api-gateway")
	if ctx.ThroughputMultiplier != 10.0 {
		t.Errorf("expected throughputMultiplier=10.0, got %f", ctx.ThroughputMultiplier)
	}
}

// 8. cascading-failure: payment service gets shouldCrash=true.
func TestFaultEngine_GetFaultContext_CascadingFailure(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	engine.Activate(simulator.FaultCascadingFailure, "payment")

	ctx := engine.GetFaultContext("payment")
	if !ctx.ShouldCrash {
		t.Error("expected shouldCrash=true for cascading-failure on payment")
	}
}

// 9. intermittent-errors: any service gets errorProbability=0.15.
func TestFaultEngine_GetFaultContext_IntermittentErrors(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	engine.Activate(simulator.FaultIntermittentErrors, "all")

	for _, svc := range []string{"order", "payment", "fulfillment", "notification", "api-gateway"} {
		ctx := engine.GetFaultContext(svc)
		if ctx.ErrorProbability != 0.15 {
			t.Errorf("service %s: expected errorProbability=0.15, got %f", svc, ctx.ErrorProbability)
		}
	}
}

// 10. No active faults → zero/false FaultContext.
func TestFaultEngine_GetFaultContext_NoFaults(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	ctx := engine.GetFaultContext("payment")
	if ctx.ShouldDropMessage || ctx.ProcessingDelayMs != 0 || ctx.ShouldCrash ||
		ctx.ErrorProbability != 0.0 || ctx.ThroughputMultiplier != 0.0 {
		t.Errorf("expected zero context with no faults active, got %+v", ctx)
	}
}

// 11. Activating an already-active fault is a no-op (origin does not change).
func TestFaultEngine_Activate_Idempotent(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	engine.Activate(simulator.FaultPoisonPill, "order")
	engine.Activate(simulator.FaultPoisonPill, "other-service") // should be ignored

	faults := engine.GetActiveFaults()
	if len(faults) != 1 {
		t.Fatalf("expected 1 active fault, got %d", len(faults))
	}
	if faults[0].Origin != "order" {
		t.Errorf("expected origin='order', got %q", faults[0].Origin)
	}
	// Only one start event should exist.
	if got := countFaultEvents(t, conn, "poison-pill", "start"); got != 1 {
		t.Errorf("expected 1 start event, got %d", got)
	}
}

// 13. Concurrent Activate/GetFaultContext does not race.
func TestFaultEngine_ConcurrentAccess(t *testing.T) {
	conn := openTestDB(t)
	engine := simulator.NewFaultEngine(conn)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			engine.Activate(simulator.FaultIntermittentErrors, "all")
		}()
		go func() {
			defer wg.Done()
			engine.GetFaultContext("payment")
		}()
	}
	wg.Wait()
}
