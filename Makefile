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

# Generate the Python SDK from the bundled OpenAPI spec via openapi-generator.
# Requires Java 8+ (openapi-generator-cli is a Java tool) and npx. CI uses the
# openapitools/openapi-generator-cli docker image instead.
SDK_SPEC := internal/apidoc/openapi.bundled.json
SDK_OUT := sdk/python
OPENAPI_GENERATOR_CLI := npx -y @openapitools/openapi-generator-cli@2.7.0

.PHONY: sdk-generate sdk-check

sdk-generate:
	$(OPENAPI_GENERATOR_CLI) generate \
	  -i $(SDK_SPEC) \
	  -g python \
	  -o $(SDK_OUT) \
	  --package-name crosslink \
	  --additional-properties=packageName=crosslink,projectName=crosslink-python
	# Normalize the license to a valid SPDX expression (PEP 639): the generator
	# copies info.license.name verbatim ("Apache 2.0") which modern setuptools
	# rejects; use the SPDX identifier form so `pip install` works.
	sed -i 's/^license = "Apache 2.0"$$/license = "Apache-2.0"/' $(SDK_OUT)/pyproject.toml
	@echo "SDK generated under $(SDK_OUT)/"

sdk-check: sdk-generate
	@git diff --exit-code -- $(SDK_OUT) || { \
		echo ""; \
		echo "ERROR: $(SDK_OUT) is stale. Run 'make sdk-generate' and commit."; \
		exit 1; }
