// Command monitor reads metrics from the shared SQLite database, detects
// anomalies, runs the correlation engine, and serves an HTTP API on :3001.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/parkerg/monitower/internal/db"
	"github.com/parkerg/monitower/internal/monitor"
)

func main() {
	dbPath := os.Getenv("MONITOWER_DB")
	if dbPath == "" {
		dbPath = "monitower.db"
	}

	conn, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	broadcaster := monitor.NewSSEBroadcaster()
	poller := monitor.NewPoller(conn, broadcaster)
	incStore := monitor.NewIncidentStore(conn)
	server := monitor.NewServer(poller, incStore, broadcaster)

	go poller.Start()

	addr := ":3001"
	log.Printf("monitor listening on %s", addr)
	if err := http.ListenAndServe(addr, server.Handler()); err != nil {
		log.Fatalf("http server: %v", err)
	}
}
