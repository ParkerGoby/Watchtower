package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/parkerg/monitower/internal/db"
)

func TestOpen_CreatesSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	conn, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	tables := []string{"service_metrics", "queue_metrics", "incidents", "fault_events"}
	for _, table := range tables {
		var name string
		err := conn.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

func TestOpen_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	for i := range 3 {
		conn, err := db.Open(path)
		if err != nil {
			t.Fatalf("Open attempt %d: %v", i, err)
		}
		conn.Close()
	}
}

func TestOpen_WALMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.db")

	conn, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	var mode string
	if err := conn.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("expected WAL mode, got %q", mode)
	}
}

func TestOpen_MissingDir(t *testing.T) {
	path := "/nonexistent/path/test.db"
	_, err := db.Open(path)
	if err == nil {
		t.Error("expected error for missing directory, got nil")
		os.Remove(path)
	}
}
