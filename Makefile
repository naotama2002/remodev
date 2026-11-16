GO_IMAGE ?= golang:1.27-bookworm
GO = docker run --rm -v $(CURDIR):/src -v remodev-gocache:/root/.cache -w /src $(GO_IMAGE)

.PHONY: images up down test test-go test-web test-e2e vet fmt
images:
	docker build -t remodev-nono:dev images/nono
	docker build -t remodev-sandbox:dev images/sandbox

up: images
	docker compose up --build -d

down:
	docker compose down

# All tests. Host needs only Docker.
test: test-go test-web test-e2e

test-go:
	$(GO) go test -race ./...

NODE = docker run --rm -v $(CURDIR)/web:/web -v remodev-pnpm:/root/.local/share/pnpm -w /web -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 node:24-bookworm-slim sh -c
test-web:
	$(NODE) 'corepack enable >/dev/null 2>&1 && pnpm install --frozen-lockfile --silent && pnpm typecheck && pnpm test'

test-e2e:
	./scripts/test-e2e.sh

# Run the control plane + UI with simulated sandboxes/GitHub/Claude (no credentials).
fake:
	docker run --rm -it -p 127.0.0.1:8081:8080 -e REMODEV_RUNTIME=fake -e REMODEV_ALLOWED_REPOS='*/*' remodev-control:dev

vet:
	$(GO) go vet ./...

fmt:
	$(GO) gofmt -w .
