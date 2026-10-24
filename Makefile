GO ?= go
PYTHON ?= python3
VERSION ?= 0.2.0

.PHONY: all build test test-go test-py demo fmt vet clean

all: build test

build:
	$(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/capledger ./cmd/capledger

test: test-go test-py

test-go: vet
	$(GO) test ./...

vet:
	@test -z "$$(gofmt -l cmd internal)" || (echo "run make fmt"; gofmt -l cmd internal; exit 1)
	$(GO) vet ./...

test-py:
	cd harness && $(PYTHON) -m pytest -q

demo:
	./scripts/demo.sh

fmt:
	gofmt -w cmd internal

clean:
	rm -rf bin
