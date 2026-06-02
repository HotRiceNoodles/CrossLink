.PHONY: build run test test-integration test-integration-oceanbase lint clean release

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -o bin/crosslink ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./...

test-integration:
	docker compose -f deployments/docker-compose.test.yaml up -d --wait
	go test -tags=integration ./internal/dialect/ -v -timeout 120s
	docker compose -f deployments/docker-compose.test.yaml down -v

test-integration-oceanbase:
	docker compose -f deployments/docker-compose.oceanbase.yaml up -d --wait
	go test -tags=integration ./internal/dialect/ -v -run TestOceanBase -timeout 120s
	docker compose -f deployments/docker-compose.oceanbase.yaml down -v

lint:
	golangci-lint run

clean:
	rm -rf bin/ build/

release:
	bash scripts/build-release.sh $(VERSION)
