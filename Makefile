GO ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: tidy vet test build lint

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
