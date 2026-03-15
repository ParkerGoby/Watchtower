.PHONY: dev build test lint fmt clean

# Run all three processes concurrently (requires make 4.x or parallel shell)
dev:
	@echo "Starting simulator, monitor, and dashboard..."
	@trap 'kill 0' INT; \
	  MONITOWER_DB=monitower.db go run ./cmd/simulator & \
	  MONITOWER_DB=monitower.db go run ./cmd/monitor & \
	  pnpm --filter dashboard dev & \
	  wait

build:
	go build ./cmd/simulator ./cmd/monitor
	pnpm --filter dashboard build

test:
	go test ./...
	pnpm --filter dashboard test:e2e

test-go:
	go test ./...

test-e2e:
	pnpm --filter dashboard test:e2e

lint:
	golangci-lint run
	pnpm --filter dashboard lint
	pnpm --filter dashboard typecheck

fmt:
	gofmt -w .

clean:
	rm -f monitower.db simulator monitor
	pnpm --filter dashboard exec rm -rf .next
