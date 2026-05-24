.PHONY: build run test lint clean release

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -o bin/crosslink ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -rf bin/ build/

release:
	bash scripts/build-release.sh $(VERSION)
