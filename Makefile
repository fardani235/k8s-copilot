VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run test race vet check live clean

build: ## build ./bin/k8s-copilot
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/k8s-copilot ./cmd/k8s-copilot

run: ## run against the current kubeconfig context
	go run ./cmd/k8s-copilot

test: ## unit tests (no cluster, no model needed)
	go test ./...

race: ## the concurrent packages under the race detector
	go test -race ./internal/agent/ ./internal/tui/ ./internal/audit/ ./internal/tools/ ./internal/kube/ ./internal/metrics/

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

check: vet test ## what CI should run

live: ## read-only + dry-run checks against the CURRENT kubeconfig context
	K8S_COPILOT_LIVE=1 go test ./internal/tools -run Live -v -count=1

clean:
	rm -rf bin
