// Package simulator contains the service workers, queue implementations,
// and fault engine that make up the Watchtower simulation environment.
package simulator

import (
	"database/sql"
	"sync"
	"time"
)

// FaultType identifies a named fault scenario.
type FaultType string

const (
	FaultPoisonPill         FaultType = "poison-pill"
	FaultDownstreamTimeout  FaultType = "downstream-timeout"
	FaultTrafficSpike       FaultType = "traffic-spike"
	FaultCascadingFailure   FaultType = "cascading-failure"
	FaultIntermittentErrors FaultType = "intermittent-errors"
)

// FaultContext describes how a service should behave given the active faults.
type FaultContext struct {
	ShouldDropMessage    bool
	ProcessingDelayMs    int
	ShouldCrash          bool
	ErrorProbability     float64
	ThroughputMultiplier float64
}

// ActiveFault holds a fault type and the service it was injected into.
type ActiveFault struct {
	Type   FaultType
	Origin string
}

// FaultEngine tracks active faults and translates them into per-service
// FaultContext values. It is safe for concurrent use.
type FaultEngine struct {
	mu     sync.RWMutex
	active map[FaultType]string // fault → origin service
	db     *sql.DB
}

// NewFaultEngine creates a FaultEngine backed by the given database connection.
func NewFaultEngine(conn *sql.DB) *FaultEngine {
	return &FaultEngine{
		active: make(map[FaultType]string),
		db:     conn,
	}
}

// Activate enables a fault and writes a fault_events row with action='start'.
// If the fault is already active, the call is a no-op.
func (e *FaultEngine) Activate(fault FaultType, origin string) {
	e.mu.Lock()
	if _, already := e.active[fault]; already {
		e.mu.Unlock()
		return
	}
	e.active[fault] = origin
	e.mu.Unlock()

	e.writeFaultEvent(fault, "start", origin)
}

// Deactivate disables a fault and writes a fault_events row with action='stop'.
func (e *FaultEngine) Deactivate(fault FaultType) {
	e.mu.Lock()
	origin := e.active[fault]
	delete(e.active, fault)
	e.mu.Unlock()

	e.writeFaultEvent(fault, "stop", origin)
}

// IsFaultActive reports whether the given fault is currently active.
func (e *FaultEngine) IsFaultActive(fault FaultType) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.active[fault]
	return ok
}

// GetActiveFaults returns a snapshot of all currently active faults.
func (e *FaultEngine) GetActiveFaults() []ActiveFault {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]ActiveFault, 0, len(e.active))
	for f, origin := range e.active {
		out = append(out, ActiveFault{Type: f, Origin: origin})
	}
	return out
}

// GetFaultContext returns the FaultContext for a service given currently active faults.
// Faults compose additively: if multiple faults affect the same service, all
// context fields from each fault are applied.
func (e *FaultEngine) GetFaultContext(service string) FaultContext {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var ctx FaultContext
	for fault := range e.active {
		apply(&ctx, fault, service)
	}
	return ctx
}

// apply merges the effect of one fault on a specific service into ctx.
func apply(ctx *FaultContext, fault FaultType, service string) {
	switch fault {
	case FaultPoisonPill:
		if service == "payment" {
			ctx.ErrorProbability = 1.0
		}
	case FaultDownstreamTimeout:
		if service == "payment" {
			ctx.ProcessingDelayMs = 8000
		}
	case FaultTrafficSpike:
		if service == "api-gateway" {
			ctx.ThroughputMultiplier = 10.0
		}
	case FaultCascadingFailure:
		if service == "payment" {
			ctx.ShouldCrash = true
		}
	case FaultIntermittentErrors:
		ctx.ErrorProbability = 0.15
	}
}

func (e *FaultEngine) writeFaultEvent(fault FaultType, action, origin string) {
	ts := time.Now().UnixMilli()
	_, _ = e.db.Exec(
		`INSERT INTO fault_events (ts, fault_type, action, origin) VALUES (?, ?, ?, ?)`,
		ts, string(fault), action, origin,
	)
}
