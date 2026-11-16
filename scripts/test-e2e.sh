#!/bin/sh
# Browser E2E tests: build the control plane, then drive the UI with Chromium
# against REMODEV_RUNTIME=fake. Runs inside the sandbox image (node + chromium),
# so the host needs only Docker.
set -eu
cd "$(dirname "$0")/.."
GO_IMAGE=${GO_IMAGE:-golang:1.27-bookworm}
SANDBOX_IMAGE=${SANDBOX_IMAGE:-remodev-sandbox:dev}
docker run --rm -v "$PWD:/repo" -v remodev-pnpm:/root/.local/share/pnpm -w /repo/web \
  -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 node:24-bookworm-slim \
  sh -c 'corepack enable >/dev/null 2>&1 && pnpm install --frozen-lockfile --silent && pnpm build >/dev/null'
docker run --rm -v "$PWD:/src" -v remodev-gocache:/root/.cache -w /src -e CGO_ENABLED=0 "$GO_IMAGE" \
  go build -buildvcs=false -o bin/remodev ./cmd/remodev
docker run --rm -v "$PWD:/repo" -w /repo/web -e REMODEV_BIN=/repo/bin/remodev \
  --user root --entrypoint node "$SANDBOX_IMAGE" --test --test-concurrency=1 "e2e/**/*.test.ts"
