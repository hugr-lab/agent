.PHONY: build run run-devui run-console test vet lint check clean

BINARY := bin/hugen
TAGS   := duckdb_arrow

# Debug-friendly CGO flags (DuckDB symbols visible in delve / stack traces).
CGO_DEBUG_FLAGS := -O1 -g

build:
	go build -tags=$(TAGS) -o $(BINARY) ./cmd/agent

build-debug:
	CGO_CFLAGS="$(CGO_DEBUG_FLAGS)" go build -tags=$(TAGS) -gcflags="all=-N -l" -o $(BINARY) ./cmd/agent

run:
	go run -tags=$(TAGS) ./cmd/agent

run-devui:
	go run -tags=$(TAGS) ./cmd/agent devui

run-console:
	go run -tags=$(TAGS) ./cmd/agent console

test:
	CGO_CFLAGS="$(CGO_DEBUG_FLAGS)" go test -tags=$(TAGS) -race -count=1 ./...

vet:
	go vet -tags=$(TAGS) ./...

lint:
	golangci-lint run --build-tags=$(TAGS) ./...

check: vet test

clean:
	rm -rf bin/
