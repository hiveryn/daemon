GO ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: tidy vet test build lint db-clean

tidy:
	$(GO) mod tidy

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

build:
	$(GO) build ./...

lint:
	$(GOLANGCI_LINT) run ./...

db-clean:
	rm -f "$$HOME/.hiveryn/daemon.db"
