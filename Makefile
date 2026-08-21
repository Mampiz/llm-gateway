.PHONY: run build test test-race e2e cover cover-html fmt fmt-check vet lint lint-go vuln trivy tidy ci smoke help  build-image

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

## e2e: build the real binary and drive it against fake upstreams
e2e:
	go test -tags e2e ./test/e2e/ -count=1 -v

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

## lint-go: run golangci-lint (installed on demand)
lint-go:
	@command -v golangci-lint >/dev/null 2>&1 || \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	golangci-lint run --timeout=5m

## vuln: report vulnerabilities reachable from this binary
vuln:
	@command -v govulncheck >/dev/null 2>&1 || \
		go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

## trivy: scan the container image for known CVEs
trivy: build-image
	trivy image --severity CRITICAL,HIGH --ignore-unfixed llm-gateway:local

## build-image: build the container image locally
build-image:
	docker build -t llm-gateway:local .

## lint: formatting and static checks together
lint: fmt-check vet lint-go

## tidy: sync go.mod with the imports actually used
tidy:
	go mod tidy

## ci: everything the pipeline runs, locally
ci: lint test-race e2e vuln build

## smoke: send one request to a gateway already running on :8080
smoke:
	curl -sS localhost:8080/v1/chat/completions \
		-H 'Content-Type: application/json' \
		-d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"say hi"}]}' | jq .

## help: list the available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
