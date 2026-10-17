GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
GOVULNCHECK   := go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
IT_SESSION    ?= delamain-test

.PHONY: build test lint vuln it check

build:
	go build -o bin/delamain ./cmd/delamain

test:
	timeout 180s go test -race ./...

lint:
	timeout 600s $(GOLANGCI_LINT) run ./...

vuln:
	timeout 300s $(GOVULNCHECK) ./...

# Requires an isolated server: herdr --session $(IT_SESSION) server
it:
	DELAMAIN_IT_HERDR_SESSION=$(IT_SESSION) timeout 120s go test ./internal/herdr/ -run Integration -v

check: lint test vuln
