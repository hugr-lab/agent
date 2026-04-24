.PHONY: build run run-devui run-console test vet lint check clean scenario

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

# Run data-driven scenarios against the live LM Studio / hub configured
# via .env (LLM_LOCAL_URL + EMBED_LOCAL_URL required). Scenarios live
# in tests/scenarios/<name>/scenario.yaml.
#
# Usage:
#   make scenario              # runs every scenario
#   make scenario name=simple  # runs just tests/scenarios/simple
#
# Each run leaves hub.db under tests/scenarios/.data/<name>-<ts>/
# memory.db for manual DuckDB inspection.
scenario:
	CGO_CFLAGS="$(CGO_DEBUG_FLAGS)" SCENARIO_NAME="$(name)" \
	go test -tags='$(TAGS) scenario' -count=1 -v -timeout=600s \
		-run "TestScenarios" ./tests/scenarios/...
