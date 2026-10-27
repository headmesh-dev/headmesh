# Headmesh agent sandbox image (plan §6.2).
# Build context: sandbox/ (the Go module root). From the repo root:
#   docker buildx bake -f sandbox/image/docker-bake.hcl sandbox-local
# Every external input is pinned by digest or verified against a sha256.

ARG UBUNTU_IMAGE=ubuntu:26.04@sha256:19fcd1bf268f02b60aca3bd30bd8a6d9a11759a430e32921833e65f16d87348a
ARG TAILSCALE_IMAGE=tailscale/tailscale:v1.104.2@sha256:b11583ad2e93e4671128538c88cf11f460444a5d7e449311dd1933a400ab849d
ARG TUNNEL_CLIENT_IMAGE=ghcr.io/openai/tunnel-client:0.0.18@sha256:486321616c197fed2d3fbe7d44f22433ce9a1e6c1ee434c0d6652868a5fda5c1
ARG GO_IMAGE=golang:1.26.8@sha256:eb36c1664dd974cde625f736e02c204383deebe03977365caaec5bf49f794348

FROM ${TAILSCALE_IMAGE} AS tailscale
FROM ${TUNNEL_CLIENT_IMAGE} AS tunnel-client

# ---------------------------------------------------------------------------
# Headmesh binaries: headmesh-sandboxd (daemon + in-container helper) and
# headmesh-sandbox (host CLI, shipped for `acceptance run` inside the sandbox).
FROM ${GO_IMAGE} AS gobuild
WORKDIR /src
ENV CGO_ENABLED=0 GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out \
 && go build -trimpath -ldflags="-s -w -buildid= -X main.version=${VERSION}" -o /out/ \
      ./cmd/headmesh-sandboxd ./cmd/headmesh-sandbox

# Build gitleaks from its pinned Go module with the same patched Go toolchain.
# The upstream release binary was built with a Go stdlib containing a critical TLS CVE.
FROM ${GO_IMAGE} AS gitleaks-build
ARG GITLEAKS_VERSION=8.30.1
ENV CGO_ENABLED=0 GOWORK=off GOTOOLCHAIN=local
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go install -trimpath \
      -ldflags="-s -w -X=github.com/zricethezav/gitleaks/v8/version.Version=${GITLEAKS_VERSION}" \
      "github.com/zricethezav/gitleaks/v8@v${GITLEAKS_VERSION}" \
 && install -D -m 0755 /go/bin/gitleaks /out/bin/gitleaks

# ---------------------------------------------------------------------------
# Downloads, each checked against a pinned sha256.
FROM ${UBUNTU_IMAGE} AS fetch
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# Versions are pinned by the image digest; the packages here never reach the final image.
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
 && rm -rf /var/lib/apt/lists/*

ARG S6_OVERLAY_VERSION=3.2.3.2
ARG S6_OVERLAY_NOARCH_SHA256=5379750ed30a84bbd2e2dd74847ba6b5bd29cd0b2e3ea2ec58049b57eb2eda12
ARG S6_OVERLAY_X86_64_SHA256=e6befcc96a437a3831386ecfc51808c5d3e939dc5fe3c02ae9284599e8aa2408
ARG S6_OVERLAY_SYMLINKS_SHA256=a215675c375aca9efecde3065df22b19fb8dcdc1362566931c6b5e778099a0fb
ARG MISE_VERSION=2026.10.20
ARG MISE_SHA256=2eb27467b6f369915ee76f18d66a0696cdb5c20e7ecc8cb49c2bfaca60f0fc9c
ARG CLAUDE_CODE_VERSION=2.1.293
ARG CLAUDE_CODE_SHA256=8968405e26db478af44eabc4635ab5ca557057b702a54460a59c13e1b253e978

WORKDIR /tmp/fetch
RUN set -eux; \
    mkdir -p /out/s6 /out/bin; \
    s6="https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}"; \
    curl -fsSLo noarch.tar.xz   "${s6}/s6-overlay-noarch.tar.xz"; \
    curl -fsSLo x86_64.tar.xz   "${s6}/s6-overlay-x86_64.tar.xz"; \
    curl -fsSLo symlinks.tar.xz "${s6}/s6-overlay-symlinks-noarch.tar.xz"; \
    printf '%s  %s\n' \
      "${S6_OVERLAY_NOARCH_SHA256}" noarch.tar.xz \
      "${S6_OVERLAY_X86_64_SHA256}" x86_64.tar.xz \
      "${S6_OVERLAY_SYMLINKS_SHA256}" symlinks.tar.xz | sha256sum -c -; \
    for t in noarch x86_64 symlinks; do tar -C /out/s6 -Jxpf "${t}.tar.xz"; done; \
    curl -fsSLo mise "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-x64"; \
    echo "${MISE_SHA256}  mise" | sha256sum -c -; \
    install -m 0755 mise /out/bin/mise; \
    curl -fsSLo claude "https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION}/linux-x64/claude"; \
    echo "${CLAUDE_CODE_SHA256}  claude" | sha256sum -c -; \
    install -m 0755 claude /out/bin/claude; \
    rm -rf /tmp/fetch/*
COPY --from=gitleaks-build --chmod=0755 /out/bin/gitleaks /out/bin/gitleaks

# ---------------------------------------------------------------------------
FROM ${UBUNTU_IMAGE} AS sandbox
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG AGENT_UID=1000
ARG AGENT_GID=1000
ARG TUNNEL_UID=990
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=1970-01-01T00:00:00Z

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8 \
    TZ=America/Toronto

# Package list from plan §6.2. Versions come from the pinned ubuntu:26.04 digest plus
# resolute-updates/security at build time; pinning each deb would break on every
# security update, so DL3008 is ignored (see .hadolint.yaml).
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git git-lfs openssh-client tmux ripgrep fd-find jq \
      build-essential pkg-config libssl-dev python3 python3-venv python3-dev \
      sqlite3 postgresql-client redis-tools iproute2 nftables iptables dnsutils \
      iputils-ping netcat-openbsd procps lsof less nano unzip xz-utils zstd \
      locales tzdata gnupg util-linux \
 && sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen \
 && locale-gen \
 && ln -sf /usr/share/zoneinfo/America/Toronto /etc/localtime \
 && echo America/Toronto > /etc/timezone \
 && ln -sf /usr/bin/fdfind /usr/local/bin/fd \
 && rm -rf /var/lib/apt/lists/*

# Users: the base image's `ubuntu` user holds UID 1000, so it goes first.
RUN if id ubuntu >/dev/null 2>&1; then userdel --remove ubuntu; fi \
 && if getent group ubuntu >/dev/null; then groupdel ubuntu; fi \
 && groupadd --gid "${AGENT_GID}" agent \
 && useradd --uid "${AGENT_UID}" --gid agent --create-home --home-dir /home/agent \
      --shell /bin/bash agent \
 && groupadd --system --gid "${TUNNEL_UID}" hmtunnel \
 && useradd --system --uid "${TUNNEL_UID}" --gid hmtunnel --home-dir /nonexistent \
      --no-create-home --shell /usr/sbin/nologin hmtunnel

COPY --from=fetch /out/s6/ /
COPY --from=fetch /out/bin/ /usr/local/bin/
COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscaled /usr/local/bin/
COPY --from=tunnel-client /usr/bin/tunnel-client /usr/local/bin/tunnel-client

# Toolchains (plan §6.2): baked as mise "system" installs under /usr/local/share/mise so
# they ship with the image; agents' extra tools go to the /cache/mise volume
# (MISE_DATA_DIR). Stable symlinks in /usr/local/share/mise/bin put the baked
# toolchains on PATH for every process, login shell or not.
COPY image/rootfs/etc/mise/config.toml /etc/mise/config.toml
ARG NPM_VERSION=12.2.1
ARG CODEX_VERSION=0.164.0
RUN --mount=type=secret,id=github_token,env=GITHUB_TOKEN \
    set -eux; \
    export MISE_DATA_DIR=/tmp/mise-data MISE_CACHE_DIR=/tmp/mise-cache MISE_YES=1; \
    mise install --system; \
    mkdir -p /usr/local/share/mise/bin; \
    node_bin="$(mise where node)/bin"; \
    PATH="${node_bin}:${PATH}" npm install --global --no-fund --no-audit \
      --cache /tmp/npm-cache "npm@${NPM_VERSION}" "@openai/codex@${CODEX_VERSION}"; \
    for d in $(mise bin-paths); do \
      for f in "$d"/*; do \
        if [ -x "$f" ] && [ ! -d "$f" ]; then ln -sf "$f" "/usr/local/share/mise/bin/$(basename "$f")"; fi; \
      done; \
    done; \
    rm -rf /tmp/mise-data /tmp/mise-cache /tmp/npm-cache /root/.npm /root/.cache

COPY image/rootfs/ /

# Permissions are set explicitly so they don't depend on the checkout's file modes.
RUN set -eux; \
    chmod 0755 /usr/local/bin/claude-session /usr/local/bin/headmesh \
      /usr/local/libexec/headmesh/git-credential-headmesh \
      /etc/headmesh/git-hooks/* /etc/headmesh/runner-hooks/* \
      /etc/s6-overlay/scripts/*; \
    find /etc/s6-overlay/s6-rc.d -type f \( -name run -o -name finish \) -exec chmod 0755 {} +; \
    chmod 0644 /etc/gitconfig /etc/profile.d/headmesh.sh /etc/ssh/ssh_known_hosts \
      /etc/headmesh/tmux.conf /etc/headmesh/agent-guidelines.md.tmpl \
      /etc/headmesh/tunnel/sandbox.yaml /etc/mise/config.toml; \
    mkdir -p /cache/mise /cache/uv /cache/npm /cache/npm-global /cache/pnpm /cache/go /cache/cargo \
      /var/lib/headmesh/sessions /var/lib/headmesh/artifacts /var/log/headmesh \
      /run/headmesh /srv/runner /srv/runner-work /etc/headmesh/config; \
    chown -R agent:agent /cache /var/lib/headmesh /var/log/headmesh /run/headmesh /srv/runner-work; \
    install -d -o agent -g agent -m 0700 /home/agent/.claude /home/agent/.codex; \
    find / -xdev -type f -perm /6000 -exec chmod ug-s {} +

COPY --from=gobuild --chmod=0755 /out/headmesh-sandboxd /out/headmesh-sandbox /usr/local/bin/
RUN ln -sf headmesh-sandboxd /usr/local/bin/hm

ENV PATH=/usr/local/share/mise/bin:/command:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    MISE_DATA_DIR=/cache/mise \
    MISE_CACHE_DIR=/cache/mise/cache \
    UV_CACHE_DIR=/cache/uv \
    UV_TOOL_DIR=/cache/uv/tools \
    UV_PYTHON_INSTALL_DIR=/cache/uv/python \
    npm_config_cache=/cache/npm \
    NPM_CONFIG_PREFIX=/cache/npm-global \
    npm_config_store_dir=/cache/pnpm/store \
    GOPATH=/cache/go \
    GOMODCACHE=/cache/go/pkg/mod \
    GOCACHE=/cache/go/build \
    CARGO_HOME=/cache/cargo \
    DISABLE_AUTOUPDATER=1 \
    HEADMESH_SANDBOX_CONFIG=/etc/headmesh/config/sandbox.toml \
    TS_NO_LOGS_NO_SUPPORT=true \
    S6_BEHAVIOUR_IF_STAGE2_FAILS=2 \
    S6_KILL_GRACETIME=3000 \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=120000 \
    S6_KEEP_ENV=1 \
    S6_VERBOSITY=1
# Shutdown: s6-rc stops every service first (`s6-rc -bda change`, no overall
# timeout), and claude-runner gets up to 80 s (its timeout-kill) to drain: that is
# the 80 s budget plan §6.7/§7.1 asks for. S6_KILL_GRACETIME is only the final
# SIGTERM-to-SIGKILL wait for leftover processes, and s6-linux-init-shutdownd
# sleeps it in full, so 80000 there made every stop take 80 s with nothing left
# to kill. TS_NO_LOGS_NO_SUPPORT keeps the tailscale CLI from uploading logs to
# Tailscale Inc. (tailscaled itself runs with --no-logs-no-support).

# Smoke test the pinned tool versions at build time.
RUN set -eux; \
    tailscale version | head -1 | grep -qx '1.104.2'; \
    HOME=/tmp claude --version | grep -q '^2.1.293 '; \
    HOME=/tmp codex --version | grep -qx 'codex-cli 0.164.0'; \
    node --version | grep -qx 'v24.16.0'; \
    npm --version | grep -qx '12.2.1'; \
    go version | grep -q 'go1.26.8 '; \
    python3.14 --version | grep -qx 'Python 3.14.4'; \
    uv --version | grep -q '^uv 0.12.24'; \
    pnpm --version | grep -qx '10.34.9'; \
    gh --version | head -1 | grep -q 'gh version 2.103.3 '; \
    gitleaks version | grep -qx '8.30.1'; \
    tunnel-client --version | grep -q '0.0.18'; \
    mise --version | grep -q '^2026.10.20 '; \
    rm -rf /tmp/.claude* /tmp/.codex /tmp/.config /tmp/.cache /tmp/.local

LABEL org.opencontainers.image.title="headmesh-sandbox" \
      org.opencontainers.image.description="Persistent agent sandbox that joins a Headscale tailnet as its own node" \
      org.opencontainers.image.source="https://github.com/headmesh-dev/headmesh" \
      org.opencontainers.image.url="https://github.com/headmesh-dev/headmesh/tree/main/sandbox" \
      org.opencontainers.image.vendor="Xcelsior Compute Inc." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.base.name="docker.io/library/ubuntu:26.04"

HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
  CMD ["headmesh-sandboxd", "health", "--timeout", "5s"]

STOPSIGNAL SIGTERM
ENTRYPOINT ["/init"]
