package simulator

import (
	"encoding/json"
	"net/http"
)

// faultOrigins maps each known fault type to the service where it originates.
var faultOrigins = map[FaultType]string{
	FaultPoisonPill:          "order",
	FaultDownstreamTimeout:   "payment",
	FaultTrafficSpike:        "api-gateway",
	FaultCascadingFailure:    "payment",
	FaultIntermittentErrors:  "payment",
	FaultNotificationFailure: "notification",
}

// NewSimulatorHandler returns an http.Handler that exposes the simulator HTTP API.
func NewSimulatorHandler(engine *FaultEngine) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/faults/inject", handleInject(engine))
	mux.HandleFunc("POST /api/faults/stop", handleStop(engine))
	mux.HandleFunc("GET /api/faults/active", handleActive(engine))
	mux.HandleFunc("POST /api/reset", handleReset(engine))
	return mux
}

func handleInject(engine *FaultEngine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FaultType string `json:"faultType"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FaultType == "" {
			http.Error(w, `{"error":"faultType is required"}`, http.StatusBadRequest)
			return
		}
		ft := FaultType(req.FaultType)
		origin, known := faultOrigins[ft]
		if !known {
			http.Error(w, `{"error":"unknown faultType"}`, http.StatusBadRequest)
			return
		}
		engine.Activate(ft, origin)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func handleStop(engine *FaultEngine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, af := range engine.GetActiveFaults() {
			engine.Deactivate(af.Type)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func handleActive(engine *FaultEngine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		faults := engine.GetActiveFaults()
		out := make([]map[string]string, len(faults))
		for i, af := range faults {
			out[i] = map[string]string{
				"type":   string(af.Type),
				"origin": af.Origin,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func handleReset(engine *FaultEngine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, af := range engine.GetActiveFaults() {
			engine.Deactivate(af.Type)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
