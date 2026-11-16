# Sandbox: where Claude Code works. Holds NO real credentials.
# All egress goes through the per-session nono proxy (HTTPS_PROXY).
FROM node:24-bookworm@sha256:9d266751ca3d7fdbe651c878ffd5ad0a42fed49a18c283509267c97f025b7496
ARG CLAUDE_VERSION=2.1.325
ARG GH_VERSION=2.104.0
ARG TARGETARCH

RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
      ca-certificates curl git jq less procps ripgrep unzip zip python3 python3-pip python3-venv build-essential \
    >/dev/null && rm -rf /var/lib/apt/lists/*

# Claude Code: native single binary, verified against the release manifest checksum.
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) p=linux-x64;   sum=0dc4f4a9a165fc60746d7ae8489712e5fc352f1e7ad4eb22e7141bd0812eb0cf ;; \
      arm64) p=linux-arm64; sum=7b6e484b1989a5a14a4b354709ccc8672518a3ec5b9ccd7981fc6254327fde67 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/claude "https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/$p/claude"; \
    echo "$sum  /usr/local/bin/claude" | sha256sum -c -; chmod 755 /usr/local/bin/claude

# GitHub CLI (read-mostly use; PRs go through remodev-pr).
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) sum=98cb101ad87dbbd3f6b2f2bfcbd43197ccd07643140121d8a9cfaa91eadbc24e ;; \
      arm64) sum=cb34cefc6f4e77b90731374a2b737516bb8f3033c6ca46aa949e5344f68069e3 ;; \
    esac; \
    curl -fsSL -o /tmp/gh.tgz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    echo "$sum  /tmp/gh.tgz" | sha256sum -c -; \
    tar -xzf /tmp/gh.tgz -C /tmp; mv /tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh /usr/local/bin/gh; rm -rf /tmp/gh*

RUN useradd -m -u 1001 -s /bin/bash agent && mkdir -p /workspace /etc/remodev && chown agent /workspace \
 # Build-time default (system CAs only) so `.remodev/Dockerfile` builds can use
 # curl/git; the entrypoint replaces it with system CAs + proxy CA at runtime.
 && cp /etc/ssl/certs/ca-certificates.crt /etc/remodev/ca-bundle.crt

COPY bin/ /usr/local/bin/
RUN chmod 755 /usr/local/bin/remodev-*

# Trust the proxy CA (cert is copied in at /etc/remodev/ca.crt before start).
ENV NODE_EXTRA_CA_CERTS=/etc/remodev/ca.crt \
    SSL_CERT_FILE=/etc/remodev/ca-bundle.crt \
    REQUESTS_CA_BUNDLE=/etc/remodev/ca-bundle.crt \
    GIT_SSL_CAINFO=/etc/remodev/ca-bundle.crt \
    NODE_USE_ENV_PROXY=1 \
    DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    IS_SANDBOX=1

ENTRYPOINT ["/usr/local/bin/remodev-entrypoint"]
