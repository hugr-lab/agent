.PHONY: build run test vet lint check clean

BINARY=bin/hugen

build:
	go build -o $(BINARY) ./cmd/agent

run:
	go run ./cmd/agent

run-devui:
	go run ./cmd/agent devui

run-console:
	go run ./cmd/agent console

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

check: vet test

clean:
	rm -rf bin/
