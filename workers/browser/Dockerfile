# Orka browser worker: Playwright's pinned Chromium behind Orka's /worker
# entrypoint. A container Task that sets this image and passes its command in
# args (leaving command empty) gets the managed worker flow: stdout becomes the
# result, /tmp/artifacts is uploaded, and a soft deadline delivers partial
# output. The Task is still treated as a custom image.
#
# The tags document human-readable versions; the manifest-list digests make
# every supported platform immutable. Keep PLAYWRIGHT_VERSION equal to
# workers/browser.PlaywrightVersion.

FROM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/worker ./workers/general

FROM docker.io/library/node:24.22.1-trixie-slim@sha256:43b9ba1142b8dcf33fa8f922ca1be85714adc444e0586c7cf7726daaeabc1bea

ARG PLAYWRIGHT_VERSION=1.64.1

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    NODE_PATH=/usr/local/lib/node_modules \
    NPM_CONFIG_AUDIT=false \
    NPM_CONFIG_FUND=false \
    NPM_CONFIG_UPDATE_NOTIFIER=false

# Playwright installs its own Chromium build and headless shell for this exact
# version, plus the system libraries they need. The fonts make screenshots of
# ordinary and emoji text legible. The node user already holds UID/GID 1000,
# which Orka forces on every Task Pod; it is renamed worker.
RUN set -eu; \
    npm install --global --no-audit --no-fund "playwright@${PLAYWRIGHT_VERSION}"; \
    test "$(playwright --version)" = "Version ${PLAYWRIGHT_VERSION}"; \
    apt-get update; \
    playwright install --with-deps chromium; \
    apt-get install -y --no-install-recommends ca-certificates git fonts-noto-core fonts-noto-color-emoji; \
    rm -rf /var/lib/apt/lists/* /root/.npm /tmp/*; \
    chmod -R a+rX "${PLAYWRIGHT_BROWSERS_PATH}"; \
    groupmod -n worker node; \
    usermod -l worker -d /tmp -s /bin/sh node; \
    printf '%s\n' \
      '#!/bin/sh' \
      '# Raw Chromium for CLI use, with the flags Orka Task Pods need.' \
      'set -eu' \
      'for chrome in /ms-playwright/chromium-*/chrome-linux*/chrome; do' \
      '  exec "$chrome" --headless --no-sandbox --disable-dev-shm-usage --disable-gpu \' \
      '    --user-data-dir=/tmp/chrome-profile "$@"' \
      'done' \
      'echo "chromium-headless: no Playwright Chromium under /ms-playwright" >&2' \
      'exit 1' \
      > /usr/local/bin/chromium-headless; \
    chmod 0755 /usr/local/bin/chromium-headless

COPY LICENSE NOTICE.md /usr/share/licenses/orka/
COPY --from=builder --chown=0:0 /out/worker /worker

LABEL org.opencontainers.image.title="Orka browser worker" \
      org.opencontainers.image.description="Playwright Chromium behind the Orka worker entrypoint for browser container Tasks" \
      org.opencontainers.image.licenses="MIT" \
      ai.orka.worker.kind="browser" \
      ai.orka.playwright.version="${PLAYWRIGHT_VERSION}"

# Orka forces UID/GID 1000 and a read-only root filesystem; only /tmp (and
# /workspace and /home/worker with a workspace) is writable.
ENV HOME=/tmp \
    XDG_CACHE_HOME=/tmp/.cache \
    XDG_CONFIG_HOME=/tmp/.config \
    NPM_CONFIG_CACHE=/tmp/npm-cache
WORKDIR /tmp
USER 1000:1000

ENTRYPOINT ["/worker"]
