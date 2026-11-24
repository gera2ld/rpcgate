# rpcgate tasks. Run `just` to list them.

# List recipes
default:
    @just --list --unsorted

# Build rpcgate into bin/
build:
    CGO_ENABLED=0 go build -trimpath -o bin/rpcgate ./cmd/rpcgate

# Run all tests (extra args go to `go test`, e.g. `just test -run Solana -v`)
test *args:
    go test ./... {{args}}

# Run tests with the race detector
race:
    go test -race -count=1 ./...

# Check formatting and run go vet
lint:
    @test -z "$(gofmt -l .)" || (echo "needs gofmt:"; gofmt -l .; exit 1)
    go vet ./...

# Format all Go files
fmt:
    gofmt -w .

# Lint and race-test: run before committing
check: lint race

# Regenerate internal/store/collections.json after editing internal/store/schema.go
schema:
    go run ./internal/store/gen internal/store/collections.json

# Run locally; data in ./pb_data. Set provider URLs in the admin UI.
run *args: build
    ./bin/rpcgate {{args}}

# Build the Docker image for this machine's platform
docker-build tag="rpcgate:latest":
    docker build -t {{tag}} .
