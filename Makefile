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

# Bundle the modular OpenAPI spec (docs/api/*) into a single embedded JSON.
# Output goes into the apidoc package so go:embed can pick it up (go:embed
# cannot traverse parent dirs). Commit the output; CI runs spec-check to fail
# if it drifts from the modular source.
SPEC_DIR := docs/api
SPEC_BUNDLE := internal/apidoc/openapi.bundled.json

.PHONY: spec-bundle spec-check

spec-bundle:
	go run ./cmd/merge-spec $(SPEC_DIR) $(SPEC_BUNDLE)

spec-check: spec-bundle
	@git diff --exit-code -- $(SPEC_BUNDLE) || { \
		echo ""; \
		echo "ERROR: $(SPEC_BUNDLE) is stale. Run 'make spec-bundle' and commit the result."; \
		exit 1; }
