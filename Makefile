.PHONY: run build test test-race cover cover-html bench fmt fmt-check vet lint tidy ci smoke help

## run: start the gateway with .env loaded if present
run:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/gateway

## build: compile the binary into bin/
build:
	go build -o bin/gateway ./cmd/gateway

## test: run the full suite
test:
	go test ./... -count=1

## test-race: run the suite under the race detector
test-race:
	go test ./... -count=1 -race

## cover: run the suite and print per-package coverage
cover:
	go test ./... -count=1 -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1

## cover-html: open the line-by-line coverage report in a browser
cover-html: cover
	go tool cover -html=coverage.out

## fmt: rewrite every file with gofmt
fmt:
	gofmt -w .

## fmt-check: fail if anything is unformatted (used by CI)
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

## vet: run the standard static checks
vet:
	go vet ./...

## lint: formatting and static checks together
lint: fmt-check vet

## tidy: sync go.mod with the imports actually used
tidy:
	go mod tidy

## ci: everything the pipeline runs, locally
ci: lint test-race build

## smoke: send one request to a gateway already running on :8080
smoke:
	curl -sS localhost:8080/v1/chat/completions \
		-H 'Content-Type: application/json' \
		-d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"say hi"}]}' | jq .

## help: list the available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
