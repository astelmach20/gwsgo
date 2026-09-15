BINARY := gwsg
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race cover lint fmt vet tidy clean install snapshot

all: fmt vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/gwsg

install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/gwsg

test:
	go test ./...

race:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -f $(BINARY) coverage.out
	rm -rf dist/
