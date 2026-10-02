.PHONY: all vet test build clean

all: vet test build

vet:
	go vet ./...

test:
	go test -race ./...

build:
	go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always 2>/dev/null || echo dev)" -o bin/agentctl .
	go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always 2>/dev/null || echo dev)" -o bin/agentflow ./cmd/agentflow

clean:
	rm -rf bin
