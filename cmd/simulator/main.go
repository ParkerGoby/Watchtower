// Command simulator runs the Monitower fault simulation environment.
// It starts all service goroutines, the fault engine, and exposes a small HTTP
// server on :3002 for receiving fault injection commands (not yet implemented).
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/parkerg/monitower/internal/db"
	"github.com/parkerg/monitower/internal/simulator"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	engine := simulator.NewFaultEngine(conn)

	orderQueue := simulator.NewQueue("order-queue", false, conn, ctx)
	paymentQueue := simulator.NewQueue("payment-queue", false, conn, ctx)
	paymentDLQ := simulator.NewQueue("payment-dlq", true, conn, ctx)
	notifSub := paymentQueue.Subscribe("notification-payment-sub", false, conn, ctx)
	notifDLQ := simulator.NewQueue("notification-dlq", true, conn, ctx)

	simulator.NewOrderService(orderQueue, engine, conn, ctx)
	simulator.NewPaymentService(orderQueue, paymentQueue, paymentDLQ, engine, conn, ctx)
	simulator.NewFulfillmentService(paymentQueue, engine, conn, ctx)
	simulator.NewNotificationService(notifSub, notifDLQ, engine, conn, ctx)

	log.Println("simulator started — HTTP API (:3002) not yet implemented")
	<-ctx.Done()
	log.Println("simulator shutting down")
}
