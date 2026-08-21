.PHONY: run build test vet fmt tidy smoke

run: ## Run the gateway with .env loaded if present
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/gateway

build:
	go build -o bin/gateway ./cmd/gateway

test:
	go test ./... -race

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

smoke: ## Send one request to a running gateway
	curl -sS localhost:8080/v1/chat/completions \
		-H 'Content-Type: application/json' \
		-d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"say hi"}]}' | jq .
