// Command simulator runs the Monitower fault simulation environment.
// It starts all service goroutines, the fault engine, and a small HTTP
// server on :3002 for receiving fault injection commands.
package main

import (
	"log"
	"os"

	"github.com/parkerg/monitower/internal/db"
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

	log.Println("simulator ready — not yet implemented")
	select {} // block until interrupted
}
