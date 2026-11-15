GO_IMAGE ?= golang:1.27-bookworm
GO = docker run --rm -v $(CURDIR):/src -v remodev-gocache:/root/.cache -w /src $(GO_IMAGE)

.PHONY: images up down test vet fmt
images:
	docker build -t remodev-nono:dev images/nono
	docker build -t remodev-sandbox:dev images/sandbox

up: images
	docker compose up --build -d

down:
	docker compose down

test:
	$(GO) go test ./...

vet:
	$(GO) go vet ./...

fmt:
	$(GO) gofmt -w .
