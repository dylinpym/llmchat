# Windows note: this host has no make(1). Every recipe below is a single plain
# command, so you can copy the text after the tab straight into PowerShell. The
# variables are just tool invocations, so `make test` and `go test -race ./...`
# are equivalent here.

GO      ?= go
GOOS    ?= linux
GOARCH  ?= amd64
BIN     ?= bin/server
GOOSE    = $(GO) run github.com/pressly/goose/v3/cmd/goose@v3.24.2
SQLC     = $(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@latest
LINT     = $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

# DSN for the migrate targets when running against compose on this host.
PGDSN    ?= host=127.0.0.1 port=5432 user=llmchat password=llmchat dbname=llmchat sslmode=disable

.PHONY: tidy fmt fmt-check vet test cover lint sqlc migrate-up migrate-down status run build compose-up compose-down logs

tidy:
	$(GO) mod tidy

fmt:
	$(GO) fmt ./...

# Prints nothing when everything is formatted; any output means a CI failure.
fmt-check:
	gofmt -l .

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

cover:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

lint:
	$(LINT) run

sqlc:
	$(SQLC) generate

migrate-up:
	$(GOOSE) -dir migrations postgres "$(PGDSN)" up

migrate-down:
	$(GOOSE) -dir migrations postgres "$(PGDSN)" down

status:
	$(GOOSE) -dir migrations postgres "$(PGDSN)" status

# Needs DATABASE_URL in the environment. LLM_PROVIDER=mock needs no API key, so
# this is the normal local target.
run:
	$(GO) run ./cmd/server

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/server

compose-up:
	docker compose up -d --build

compose-down:
	docker compose down

logs:
	docker compose logs -f server
