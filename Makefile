.PHONY: build run test test-race lint vet vuln docker-up docker-down migrate fmt help

BIN := bin/server
PKG := ./...

help:
	@echo "Targets:"
	@echo "  make build         — compile the server binary"
	@echo "  make run           — go run the server"
	@echo "  make test          — run unit tests"
	@echo "  make test-race     — run tests with race detector"
	@echo "  make lint          — staticcheck"
	@echo "  make vet           — go vet"
	@echo "  make vuln          — govulncheck"
	@echo "  make fmt           — gofmt -s -w ."
	@echo "  make docker-up     — docker compose up -d"
	@echo "  make docker-down   — docker compose down"
	@echo "  make migrate       — run migrations (requires DATABASE_URL)"

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="-w -s" -o $(BIN) ./cmd/server

run:
	go run ./cmd/server

test:
	go test -count=1 $(PKG)

test-race:
	go test -race -count=1 $(PKG)

vet:
	go vet $(PKG)

lint:
	@which staticcheck >/dev/null 2>&1 || go install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck $(PKG)

vuln:
	@which govulncheck >/dev/null 2>&1 || go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck $(PKG)

fmt:
	gofmt -s -w .

docker-up:
	docker compose up -d

docker-down:
	docker compose down

migrate:
	@which migrate >/dev/null 2>&1 || go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
	migrate -path migrations -database "$$DATABASE_URL" up
