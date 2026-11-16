# Control plane image: UI build (node, build-time only) + Go binary.
FROM node:24-bookworm-slim@sha256:dc26ae92c9fb15cf6b1d63e20f0944b2efe0f115b20e00be48fb23fceb061c54 AS ui
WORKDIR /web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml* web/pnpm-workspace.yaml* web/.npmrc* ./
RUN if [ -f package.json ]; then pnpm install --frozen-lockfile --ignore-scripts; fi
COPY web/ ./
RUN if [ -f package.json ]; then pnpm build; else mkdir -p dist; fi

FROM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY . .
COPY --from=ui /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/remodev ./cmd/remodev

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/remodev /remodev
ENTRYPOINT ["/remodev"]
