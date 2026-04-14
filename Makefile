.PHONY: build run test vet lint check clean

BINARY=bin/hugr-agent

build:
	go build -o $(BINARY) ./cmd/hugr-agent

run:
	go run ./cmd/hugr-agent

run-devui:
	go run ./cmd/hugr-agent devui

run-console:
	go run ./cmd/hugr-agent console

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

check: vet test

clean:
	rm -rf bin/
