# Sandbox environment for working on remodev itself (see docs/vision.md).
# REMODEV_BASE is the default remodev sandbox image (Claude Code, gh, node).
ARG REMODEV_BASE
FROM ${REMODEV_BASE}
ARG GO_VERSION=1.27.4
USER root
# The Docker Engine API builder does not set TARGETARCH, so detect it here.
RUN set -eu; \
    case "$(uname -m)" in \
      x86_64)  arch=amd64; sum=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840 ;; \
      aarch64) arch=arm64; sum=3392582988a273a87ab5043ee1b2c9c17a2c3c00580bf9326c47a2e0aaaeac92 ;; \
    esac; \
    curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"; \
    echo "$sum  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:/workspace/home/go/bin:$PATH \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly
