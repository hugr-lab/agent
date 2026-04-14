.PHONY: build run test vet lint check clean

BINARY=bin/agent

build:
	go build -o $(BINARY) ./cmd/agent

run:
	go run ./cmd/agent

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

check: vet test

clean:
	rm -rf bin/
