# Egress proxy: nono standalone proxy (credential injection + L7 allow list).
# Version and checksums are pinned; bump deliberately.
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS fetch
ARG NONO_VERSION=0.82.0
ARG TARGETARCH
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends ca-certificates curl >/dev/null && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) f=nono-v${NONO_VERSION}-x86_64-unknown-linux-musl.tar.gz; sum=2d81384d9025c7ff9c9e29b8f5e83d39cc6739ba37ba8a0c50fe50ceea4653de ;; \
      arm64) f=nono-v${NONO_VERSION}-aarch64-unknown-linux-gnu.tar.gz; sum=de16b663f1941b4ff8500f75f34d10a497ef1d1934281573888fb9e808dab53e ;; \
      *) echo "unsupported arch $TARGETARCH"; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/nono.tgz "https://github.com/nolabs-ai/nono/releases/download/v${NONO_VERSION}/$f"; \
    echo "$sum  /tmp/nono.tgz" | sha256sum -c -; \
    tar -xzf /tmp/nono.tgz -C /usr/local/bin nono; chmod 755 /usr/local/bin/nono

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends ca-certificates >/dev/null && rm -rf /var/lib/apt/lists/* \
 && useradd -m -u 10001 nono && mkdir -p /etc/nono && chown nono /etc/nono
COPY --from=fetch /usr/local/bin/nono /usr/local/bin/nono
USER nono
ENV RUST_LOG=info
ENTRYPOINT ["/usr/local/bin/nono"]
CMD ["proxy", "--help"]
