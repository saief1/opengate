BINARY  := opengate
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/opengate-dev/opengate/internal/server.Version=$(VERSION)
export CGO_ENABLED=0

.PHONY: build test test-race test-postgres test-integration vet fmt run mock demo docker clean zip

build: ## Build a static binary into ./bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/opengate

test: ## Unit + integration tests
	go test ./...

test-race: ## Tests with the race detector (needs CGO for -race)
	CGO_ENABLED=1 go test -race ./...

# Runs the store, budget and integration suites against PostgreSQL. Each test
# creates (and drops) its own database on this server.
OPENGATE_TEST_POSTGRES_DSN ?= postgres://opengate:opengate@localhost:5432/postgres?sslmode=disable
test-postgres: ## Tests against PostgreSQL (OPENGATE_TEST_POSTGRES_DSN)
	OPENGATE_TEST_POSTGRES_DSN="$(OPENGATE_TEST_POSTGRES_DSN)" go test -count=1 ./internal/store/ ./internal/budget/ ./integration/

test-integration:
	go test -count=1 -v ./integration/

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

fmt:
	gofmt -w .

run: build ## Run with examples/opengate.example.yaml (needs OPENGATE_ADMIN_TOKEN / OPENGATE_MASTER_KEY)
	./bin/$(BINARY) serve -config examples/opengate.example.yaml

mock: build ## Run the mock upstream provider on :9090
	./bin/$(BINARY) mock-provider -listen 127.0.0.1:9090

demo: build ## Mock provider + gateway + seeded demo traffic (no real keys needed)
	./scripts/demo.sh

docker:
	docker build -t opengate:$(VERSION) --build-arg VERSION=$(VERSION) .

zip: ## Source archive (no build artifacts)
	git archive --format=zip --prefix=opengate/ -o ../opengate.zip HEAD

clean:
	rm -rf bin dist *.db *.db-shm *.db-wal
