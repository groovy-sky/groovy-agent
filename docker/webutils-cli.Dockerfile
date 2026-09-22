# syntax=docker/dockerfile:1.7

FROM golang:1.24-bookworm AS go-builder
WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH=amd64
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
COPY webutils ./webutils
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags='-s -w' -o /out/webutils-cli ./cmd/webutils-cli

FROM debian:bookworm-slim AS chromium-debian
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends chromium chromium-sandbox; \
    mkdir -p /chromium-rootfs /chromium-rootfs/opt/chromium-libs; \
    copy_package_files() { \
      pkg="$1"; \
      dest="$2"; \
      dpkg-query -L "$pkg" | while read -r path; do \
        [ -n "$path" ] || continue; \
        [ -e "$path" ] || continue; \
        case "$path" in \
          /usr/share/doc/*|/usr/share/lintian/*|/usr/share/man/*|/usr/share/menu/*|/usr/share/bug/*) \
            continue ;; \
        esac; \
        if [ -d "$path" ]; then \
          mkdir -p "$dest$path"; \
        else \
          mkdir -p "$dest$(dirname "$path")"; \
          cp -a "$path" "$dest$path"; \
        fi; \
      done; \
    }; \
    payload_packages='chromium chromium-common chromium-sandbox'; \
    dependency_packages="$( \
      apt-cache depends --recurse --no-recommends --no-suggests --no-conflicts --no-breaks --no-replaces --no-enhances chromium chromium-common \
        | sed -n 's/^[| ]*PreDepends: //p; s/^[| ]*Depends: //p' \
        | tr -d '<>' \
        | awk '$1 != "" && $1 != "chromium" && $1 != "chromium-common" && $1 != "libc6" { print $1 }' \
        | sort -u \
    )"; \
    for pkg in $payload_packages; do \
      copy_package_files "$pkg" /chromium-rootfs; \
    done; \
    for pkg in $dependency_packages; do \
      copy_package_files "$pkg" /chromium-rootfs/opt/chromium-libs; \
    done; \
    rm -rf /var/lib/apt/lists/*

FROM debian:bookworm-slim AS runtime
ARG AGENT_UID=10001
ARG AGENT_GID=10001
ENV WEBUTILS_CHROME_EXECUTABLE=/usr/bin/chromium \
    WEBUTILS_CHROME_ARGS="--no-sandbox --disable-dev-shm-usage" \
    HOME=/home/webutils-cli \
    XDG_CONFIG_HOME=/home/webutils-cli/.config \
    XDG_CACHE_HOME=/home/webutils-cli/.cache \
    XDG_DATA_HOME=/home/webutils-cli/.local/share \
    XDG_RUNTIME_DIR=/home/webutils-cli/.local/run
COPY --from=go-builder /out/webutils-cli /usr/local/bin/webutils-cli
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        fonts-liberation \
        fonts-noto-color-emoji \
        libnspr4 \
        libnss3 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=chromium-debian /chromium-rootfs/ /
COPY docker/chromium-wrapper.sh /usr/bin/chromium
RUN chmod +x /usr/bin/chromium /usr/local/bin/webutils-cli \
    && test -x /usr/bin/chromium \
    && test -u /usr/lib/chromium/chrome-sandbox \
    && /usr/bin/chromium --version >/dev/null \
    && groupadd --gid "${AGENT_GID}" webutils-cli \
    && useradd --uid "${AGENT_UID}" --gid "${AGENT_GID}" --create-home --home-dir "${HOME}" --shell /usr/sbin/nologin webutils-cli \
    && install -d -o webutils-cli -g webutils-cli -m 0700 \
        "${XDG_CONFIG_HOME}" \
        "${XDG_CACHE_HOME}" \
        "${XDG_DATA_HOME}" \
        "${XDG_RUNTIME_DIR}"
USER ${AGENT_UID}:${AGENT_GID}
ENTRYPOINT ["/usr/local/bin/webutils-cli"]
